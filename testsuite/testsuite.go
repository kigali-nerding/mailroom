package testsuite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/nyaruka/gocommon/storage"
	"github.com/nyaruka/mailroom/config"
	"github.com/nyaruka/mailroom/runtime"

	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storageDir = "_test_storage"

// Reset clears out both our database and redis DB
func Reset() (context.Context, *sqlx.DB, *redis.Pool) {
	logrus.SetLevel(logrus.DebugLevel)
	ResetDB()
	ResetRP()

	return CTX(), DB(), RP()
}

// ResetDB resets our database to our base state from our RapidPro dump
//
// mailroom_test.dump can be regenerated by running:
//   % python manage.py mailroom_db
//
// then copying the mailroom_test.dump file to your mailroom root directory
//   % cp mailroom_test.dump ../mailroom
func ResetDB() {
	db := sqlx.MustOpen("postgres", "postgres://mailroom_test:temba@localhost/mailroom_test?sslmode=disable&Timezone=UTC")
	defer db.Close()
	db.MustExec("drop owned by mailroom_test cascade")
	dir, _ := os.Getwd()

	// our working directory is set to the directory of the module being tested, we want to get just
	// the portion that points to the mailroom directory
	for !strings.HasSuffix(dir, "mailroom") && dir != "/" {
		dir = path.Dir(dir)
	}

	mustExec("pg_restore", "-h", "localhost", "-d", "mailroom_test", "-U", "mailroom_test", path.Join(dir, "./mailroom_test.dump"))
}

// DB returns an open test database pool
func DB() *sqlx.DB {
	db := sqlx.MustOpen("postgres", "postgres://mailroom_test:temba@localhost/mailroom_test?sslmode=disable&Timezone=UTC")
	return db
}

// ResetRP resets our redis database
func ResetRP() {
	rc, err := redis.Dial("tcp", "localhost:6379")
	if err != nil {
		panic(fmt.Sprintf("error connecting to redis db: %s", err.Error()))
	}
	rc.Do("SELECT", 0)
	_, err = rc.Do("FLUSHDB")
	if err != nil {
		panic(fmt.Sprintf("error flushing redis db: %s", err.Error()))
	}
}

// RP returns a redis pool to our test database
func RP() *redis.Pool {
	return &redis.Pool{
		Dial: func() (redis.Conn, error) {
			conn, err := redis.Dial("tcp", "localhost:6379")
			if err != nil {
				return nil, err
			}
			_, err = conn.Do("SELECT", 0)
			return conn, err
		},
	}
}

// RC returns a redis connection, Close() should be called on it when done
func RC() redis.Conn {
	conn, err := redis.Dial("tcp", "localhost:6379")
	if err != nil {
		panic(err)
	}
	_, err = conn.Do("SELECT", 0)
	if err != nil {
		panic(err)
	}
	return conn
}

// CTX returns our background testing context
func CTX() context.Context {
	return context.Background()
}

// Storage returns our storage for tests
func Storage() storage.Storage {
	return storage.NewFS(storageDir)
}

// ResetStorage clears our storage for tests
func ResetStorage() {
	if err := os.RemoveAll(storageDir); err != nil {
		panic(err)
	}
}

// utility function for running a command panicking if there is any error
func mustExec(command string, args ...string) {
	cmd := exec.Command(command, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("error restoring database: %s: %s", err, string(output)))
	}
}

// AssertQueryCount can be used to assert that a query returns the expected number of
func AssertQueryCount(t *testing.T, db *sqlx.DB, sql string, args []interface{}, count int, errMsg ...interface{}) {
	var c int
	err := db.Get(&c, sql, args...)
	if err != nil {
		assert.Fail(t, "error performing query: %s - %s", sql, err)
	}
	assert.Equal(t, count, c, errMsg...)
}

// AssertCourierQueues asserts the sizes of message batches in the named courier queues
func AssertCourierQueues(t *testing.T, expected map[string][]int, errMsg ...interface{}) {
	rc := RC()
	defer rc.Close()

	queueKeys, err := redis.Strings(rc.Do("KEYS", "msgs:????????-*"))
	require.NoError(t, err)

	actual := make(map[string][]int, len(queueKeys))
	for _, queueKey := range queueKeys {
		size, err := redis.Int64(rc.Do("ZCARD", queueKey))
		require.NoError(t, err)
		actual[queueKey] = make([]int, size)

		if size > 0 {
			results, err := redis.Values(rc.Do("ZPOPMAX", queueKey, size))
			require.NoError(t, err)
			require.Equal(t, int(size*2), len(results)) // result is (item, score, item, score, ...)

			// unmarshal each item in the queue as a batch of messages
			for i := 0; i < int(size); i++ {
				batchJSON := results[i*2].([]byte)
				var batch []map[string]interface{}
				err = json.Unmarshal(batchJSON, &batch)
				require.NoError(t, err)

				actual[queueKey][i] = len(batch)
			}
		}
	}

	assert.Equal(t, expected, actual, errMsg...)
}

func Runtime() *runtime.Runtime {
	return &runtime.Runtime{
		RP:      RP(),
		DB:      DB(),
		ES:      nil,
		Storage: Storage(),
		Config:  config.NewMailroomConfig(),
	}
}
