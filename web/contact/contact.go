package contact

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/nyaruka/gocommon/urns"
	"github.com/nyaruka/goflow/assets"
	"github.com/nyaruka/goflow/contactql"
	"github.com/nyaruka/goflow/envs"
	"github.com/nyaruka/goflow/flows"
	"github.com/nyaruka/goflow/flows/actions/modifiers"
	"github.com/nyaruka/goflow/utils"
	"github.com/nyaruka/mailroom/models"
	"github.com/nyaruka/mailroom/web"

	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
)

func init() {
	web.RegisterJSONRoute(http.MethodPost, "/mr/contact/search", web.RequireAuthToken(handleSearch))
	web.RegisterJSONRoute(http.MethodPost, "/mr/contact/parse_query", web.RequireAuthToken(handleParseQuery))
	web.RegisterJSONRoute(http.MethodPost, "/mr/contact/create", web.RequireAuthToken(handleCreate))
	web.RegisterJSONRoute(http.MethodPost, "/mr/contact/modify", web.RequireAuthToken(handleModify))
}

// Searches the contacts for an org
//
//   {
//     "org_id": 1,
//     "group_uuid": "985a83fe-2e9f-478d-a3ec-fa602d5e7ddd",
//     "query": "age > 10",
//     "sort": "-age"
//   }
//
type searchRequest struct {
	OrgID     models.OrgID     `json:"org_id"     validate:"required"`
	GroupUUID assets.GroupUUID `json:"group_uuid" validate:"required"`
	Query     string           `json:"query"`
	PageSize  int              `json:"page_size"`
	Offset    int              `json:"offset"`
	Sort      string           `json:"sort"`
}

// Response for a contact search
//
// {
//   "query": "age > 10",
//   "contact_ids": [5,10,15],
//   "total": 3,
//   "offset": 0,
//   "metadata": {
//     "fields": [
//       {"key": "age", "name": "Age"}
//     ],
//     "allow_as_group": true
//   }
// }
type searchResponse struct {
	Query      string                `json:"query"`
	ContactIDs []models.ContactID    `json:"contact_ids"`
	Total      int64                 `json:"total"`
	Offset     int                   `json:"offset"`
	Sort       string                `json:"sort"`
	Metadata   *contactql.Inspection `json:"metadata,omitempty"`

	// deprecated
	Fields       []string `json:"fields"`
	AllowAsGroup bool     `json:"allow_as_group"`
}

// handles a contact search request
func handleSearch(ctx context.Context, s *web.Server, r *http.Request) (interface{}, int, error) {
	request := &searchRequest{
		Offset:   0,
		PageSize: 50,
		Sort:     "-id",
	}
	if err := utils.UnmarshalAndValidateWithLimit(r.Body, request, web.MaxRequestBytes); err != nil {
		return errors.Wrapf(err, "request failed validation"), http.StatusBadRequest, nil
	}

	// grab our org
	org, err := models.GetOrgAssetsWithRefresh(s.CTX, s.DB, request.OrgID, models.RefreshFields)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "unable to load org assets")
	}

	// Perform our search
	parsed, hits, total, err := models.ContactIDsForQueryPage(ctx, s.ElasticClient, org,
		request.GroupUUID, request.Query, request.Sort, request.Offset, request.PageSize)

	if err != nil {
		isQueryError, qerr := contactql.IsQueryError(err)
		if isQueryError {
			return qerr, http.StatusBadRequest, nil
		}
		return nil, http.StatusInternalServerError, err
	}

	// normalize and inspect the query
	normalized := ""
	var metadata *contactql.Inspection
	allowAsGroup := false
	fields := make([]string, 0)

	if parsed != nil {
		normalized = parsed.String()
		metadata = contactql.Inspect(parsed)
		fields = append(fields, metadata.Attributes...)
		for _, f := range metadata.Fields {
			fields = append(fields, f.Key)
		}
		allowAsGroup = metadata.AllowAsGroup
	}

	// build our response
	response := &searchResponse{
		Query:        normalized,
		ContactIDs:   hits,
		Total:        total,
		Offset:       request.Offset,
		Sort:         request.Sort,
		Metadata:     metadata,
		Fields:       fields,
		AllowAsGroup: allowAsGroup,
	}

	return response, http.StatusOK, nil
}

// Request to parse the passed in query
//
//   {
//     "org_id": 1,
//     "query": "age > 10",
//     "group_uuid": "123123-123-123-"
//   }
//
type parseRequest struct {
	OrgID     models.OrgID     `json:"org_id"     validate:"required"`
	Query     string           `json:"query"      validate:"required"`
	GroupUUID assets.GroupUUID `json:"group_uuid"`
}

// Response for a parse query request
//
// {
//   "query": "age > 10",
//   "elastic_query": { .. },
//   "metadata": {
//     "fields": [
//       {"key": "age", "name": "Age"}
//     ],
//     "allow_as_group": true
//   }
// }
type parseResponse struct {
	Query        string                `json:"query"`
	ElasticQuery interface{}           `json:"elastic_query"`
	Metadata     *contactql.Inspection `json:"metadata,omitempty"`

	// deprecated
	Fields       []string `json:"fields"`
	AllowAsGroup bool     `json:"allow_as_group"`
}

// handles a query parsing request
func handleParseQuery(ctx context.Context, s *web.Server, r *http.Request) (interface{}, int, error) {
	request := &parseRequest{}
	if err := utils.UnmarshalAndValidateWithLimit(r.Body, request, web.MaxRequestBytes); err != nil {
		return errors.Wrapf(err, "request failed validation"), http.StatusBadRequest, nil
	}

	// grab our org
	org, err := models.GetOrgAssetsWithRefresh(s.CTX, s.DB, request.OrgID, models.RefreshFields)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "unable to load org assets")
	}

	env := org.Env()
	parsed, err := contactql.ParseQuery(request.Query, env.RedactionPolicy(), env.DefaultCountry(), org.SessionAssets())

	if err != nil {
		isQueryError, qerr := contactql.IsQueryError(err)
		if isQueryError {
			return qerr, http.StatusBadRequest, nil
		}
		return nil, http.StatusInternalServerError, err
	}

	// normalize and inspect the query
	normalized := ""
	var metadata *contactql.Inspection
	allowAsGroup := false
	fields := make([]string, 0)

	if parsed != nil {
		normalized = parsed.String()
		metadata = contactql.Inspect(parsed)
		fields = append(fields, metadata.Attributes...)
		for _, f := range metadata.Fields {
			fields = append(fields, f.Key)
		}
		allowAsGroup = metadata.AllowAsGroup
	}

	eq, err := models.BuildElasticQuery(org, request.GroupUUID, parsed)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	eqj, err := eq.Source()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// build our response
	response := &parseResponse{
		Query:        normalized,
		ElasticQuery: eqj,
		Metadata:     metadata,
		Fields:       fields,
		AllowAsGroup: allowAsGroup,
	}

	return response, http.StatusOK, nil
}

// Request that a set of contacts is created.
//
//   {
//     "org_id": 1,
//     "user_id": 1,
//     "contacts": [{
//        "name": "Joe Blow",
//        "language": "eng",
//        "urns": ["tel:+250788123123"],
//        "fields": {"age": "39"},
//        "groups": ["b0b778db-6657-430b-9272-989ad43a10db"]
//     }, {
//        "name": "Frank",
//        "language": "spa",
//        "urns": ["tel:+250788676767", "twitter:franky"],
//        "fields": {}
//     }]
//   }
//
type createRequest struct {
	OrgID    models.OrgID  `json:"org_id"       validate:"required"`
	UserID   models.UserID `json:"user_id"`
	Contacts []struct {
		Name    string             `json:"name"`
		Languge envs.Language      `json:"language"`
		URNs    []urns.URN         `json:"urns"`
		Fields  map[string]string  `json:"fields"`
		Groups  []assets.GroupUUID `json:"groups"`
	} `json:"contacts"       validate:"required"`
}

// Response for contact creation. Will return an array of contacts/errors the same size as that in the request.
//
//   [{
//	   "contact": {
//       "id": 123,
//       "uuid": "559d4cf7-8ed3-43db-9bbb-2be85345f87e",
//       "name": "Joe",
//       "language": "eng"
//     }
//   },{
//     "error": "URNs owned by other contact"
//   }]
//
type createResult struct {
	Contact *flows.Contact `json:"contact,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// handles a request to create the given contacts
func handleCreate(ctx context.Context, s *web.Server, r *http.Request) (interface{}, int, error) {
	request := &createRequest{}
	if err := utils.UnmarshalAndValidateWithLimit(r.Body, request, web.MaxRequestBytes); err != nil {
		return errors.Wrapf(err, "request failed validation"), http.StatusBadRequest, nil
	}

	// grab our org
	org, err := models.GetOrgAssets(s.CTX, s.DB, request.OrgID)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "unable to load org assets")
	}

	results := make([]createResult, len(request.Contacts))

	for i, c := range request.Contacts {
		_, flowContact, err := models.CreateContact(ctx, s.DB, org, request.UserID, c.Name, c.Languge, c.URNs)
		if err != nil {
			results[i].Error = err.Error()
			continue
		}

		results[i].Contact = flowContact
	}

	return results, http.StatusOK, nil
}

// Request that a set of contacts is modified.
//
//   {
//     "org_id": 1,
//     "user_id": 1,
//     "contact_ids": [15,235],
//     "modifiers": [{
//        "type": "groups",
//        "modification": "add",
//        "groups": [{
//            "uuid": "a8e8efdb-78ee-46e7-9eb0-6a578da3b02d",
//            "name": "Doctors"
//        }]
//     }]
//   }
//
type modifyRequest struct {
	OrgID      models.OrgID       `json:"org_id"       validate:"required"`
	UserID     models.UserID      `json:"user_id"`
	ContactIDs []models.ContactID `json:"contact_ids"  validate:"required"`
	Modifiers  []json.RawMessage  `json:"modifiers"    validate:"required"`
}

// Response for a contact update. Will return the full contact state and any errors
//
// {
//   "1000": {
//	   "contact": {
//       "id": 123,
//       "contact_uuid": "559d4cf7-8ed3-43db-9bbb-2be85345f87e",
//       "name": "Joe",
//       "language": "eng",
//       ...
//     }],
//     "events": [{
//          ....
//     }]
//   }, ...
// }
type modifyResult struct {
	Contact *flows.Contact `json:"contact"`
	Events  []flows.Event  `json:"events"`
}

// handles a request to modify the given contacts
func handleModify(ctx context.Context, s *web.Server, r *http.Request) (interface{}, int, error) {
	request := &modifyRequest{}
	if err := utils.UnmarshalAndValidateWithLimit(r.Body, request, web.MaxRequestBytes); err != nil {
		return errors.Wrapf(err, "request failed validation"), http.StatusBadRequest, nil
	}

	// grab our org
	org, err := models.GetOrgAssets(s.CTX, s.DB, request.OrgID)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "unable to load org assets")
	}

	// clone it as we will modify flows
	org, err = org.Clone(s.CTX, s.DB)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "unable to clone orgs")
	}

	// build up our modifiers
	mods := make([]flows.Modifier, len(request.Modifiers))
	for i, m := range request.Modifiers {
		mod, err := modifiers.ReadModifier(org.SessionAssets(), m, assets.IgnoreMissing)
		if err != nil {
			return errors.Wrapf(err, "error in modifier: %s", string(m)), http.StatusBadRequest, nil
		}
		mods[i] = mod
	}

	// load our contacts
	contacts, err := models.LoadContacts(ctx, s.DB, org, request.ContactIDs)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "unable to load contact")
	}

	results := make(map[models.ContactID]modifyResult)

	// create an environment instance with location support
	env := flows.NewEnvironment(org.Env(), org.SessionAssets().Locations())

	// create scenes for our contacts
	scenes := make([]*models.Scene, 0, len(contacts))
	for _, contact := range contacts {
		flowContact, err := contact.FlowContact(org)
		if err != nil {
			return nil, http.StatusInternalServerError, errors.Wrapf(err, "error creating flow contact for contact: %d", contact.ID())
		}

		result := modifyResult{
			Contact: flowContact,
			Events:  make([]flows.Event, 0, len(mods)),
		}

		scene := models.NewSceneForContact(flowContact)

		// apply our modifiers
		for _, mod := range mods {
			mod.Apply(env, org.SessionAssets(), flowContact, func(e flows.Event) { result.Events = append(result.Events, e) })
		}

		results[contact.ID()] = result
		scenes = append(scenes, scene)
	}

	// ok, commit all our events
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "error starting transaction")
	}

	// apply our events
	for _, scene := range scenes {
		err := models.HandleEvents(ctx, tx, s.RP, org, scene, results[scene.ContactID()].Events)
		if err != nil {
			return nil, http.StatusInternalServerError, errors.Wrapf(err, "error applying events")
		}
	}

	// gather all our pre commit events, group them by hook and apply them
	err = models.ApplyEventPreCommitHooks(ctx, tx, s.RP, org, scenes)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "error applying pre commit hooks")
	}

	// commit our transaction
	err = tx.Commit()
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "error committing pre commit hooks")
	}

	tx, err = s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "error starting transaction for post commit")
	}

	// then apply our post commit hooks
	err = models.ApplyEventPostCommitHooks(ctx, tx, s.RP, org, scenes)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "error applying pre commit hooks")
	}

	err = tx.Commit()
	if err != nil {
		return nil, http.StatusInternalServerError, errors.Wrapf(err, "error committing pre commit hooks")
	}

	return results, http.StatusOK, nil
}

func modifyContacts(ctx context.Context, db *sqlx.DB, rp *redis.Pool, oa *models.OrgAssets, contacts []*flows.Contact, mods func(*flows.Contact) []flows.Modifier) error {
	// create an environment instance with location support
	env := flows.NewEnvironment(oa.Env(), oa.SessionAssets().Locations())

	// apply the modifiers to get the events for each contact
	eventsByContact := make(map[*flows.Contact][]flows.Event)
	for _, contact := range contacts {
		events := make([]flows.Event, 0)
		for _, mod := range mods(contact) {
			mod.Apply(env, oa.SessionAssets(), contact, func(e flows.Event) { events = append(events, e) })
		}
		eventsByContact[contact] = events
	}

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return errors.Wrapf(err, "error starting transaction")
	}

	scenes := make([]*models.Scene, len(contacts))

	for i, contact := range contacts {
		scenes[i] = models.NewSceneForContact(contact)

		err := models.HandleEvents(ctx, tx, rp, oa, scenes[i], eventsByContact[contact])
		if err != nil {
			return errors.Wrapf(err, "error handling events")
		}
	}

	// gather all our pre commit events, group them by hook and apply them
	err = models.ApplyEventPreCommitHooks(ctx, tx, rp, oa, scenes)
	if err != nil {
		return errors.Wrapf(err, "error applying pre commit hooks")
	}

	// commit our transaction
	if err := tx.Commit(); err != nil {
		return errors.Wrapf(err, "error committing transaction")
	}

	// start new transaction for post commit hooks
	tx, err = db.BeginTxx(ctx, nil)
	if err != nil {
		return errors.Wrapf(err, "error starting transaction for post commit")
	}

	// then apply our post commit hooks
	err = models.ApplyEventPostCommitHooks(ctx, tx, rp, oa, scenes)
	if err != nil {
		return errors.Wrapf(err, "error applying post commit hooks")
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrapf(err, "error committing post commit hooks")
	}

	return nil
}
