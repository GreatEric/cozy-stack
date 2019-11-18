package note

import (
	"fmt"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/prosemirror-go/transform"
)

// Step is a patch to apply on a note.
type Step map[string]interface{}

// ID returns the directory qualified identifier
func (s Step) ID() string {
	id, _ := s["_id"].(string)
	return id
}

// Rev returns the directory revision
func (s Step) Rev() string {
	rev, _ := s["_rev"].(string)
	return rev
}

// DocType returns the document type
func (s Step) DocType() string { return consts.NotesSteps }

// Clone implements couchdb.Doc
func (s Step) Clone() couchdb.Doc {
	cloned := make(Step)
	for k, v := range s {
		cloned[k] = v
	}
	return cloned
}

// SetID changes the step qualified identifier
func (s Step) SetID(id string) {
	if id == "" {
		delete(s, "_id")
	} else {
		s["_id"] = id
	}
}

// SetRev changes the step revision
func (s Step) SetRev(rev string) {
	if rev == "" {
		delete(s, "_rev")
	} else {
		s["_rev"] = rev
	}
}

// Included is part of the jsonapi.Object interface
func (s Step) Included() []jsonapi.Object { return nil }

// Links is part of the jsonapi.Object interface
func (s Step) Links() *jsonapi.LinksList { return nil }

// Relationships is part of the jsonapi.Object interface
func (s Step) Relationships() jsonapi.RelationshipMap { return nil }

func timeToVersion(t time.Time) int64 {
	return t.UnixNano() / 1000000
}

func stepID(noteID string, version int64) string {
	return fmt.Sprintf("%s/%d", noteID, version)
}

// GetSteps returns the steps for the given note, starting from the version.
func GetSteps(inst *instance.Instance, fileID string, version int64) ([]Step, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	return getSteps(inst, fileID, version)
}

// getSteps is the same as GetSteps, but with the notes lock already acquired
func getSteps(inst *instance.Instance, fileID string, version int64) ([]Step, error) {
	var steps []Step
	req := couchdb.AllDocsRequest{
		Limit:    1000,
		StartKey: stepID(fileID, version),
	}
	if err := couchdb.GetAllDocs(inst, consts.NotesSteps, &req, &steps); err != nil {
		return nil, err
	}

	// The first step plays the role of a sentinel: if it isn't here, the
	// version is too old. Same if we have too many steps.
	if len(steps) == 0 || len(steps) == req.Limit {
		return nil, ErrTooOld
	}

	steps = steps[1:] // Discard the sentinel
	return steps, nil
}

// ApplySteps takes a note and some steps, and tries to apply them. It is an
// all or nothing change: if there is one error, the note won't be changed.
func ApplySteps(inst *instance.Instance, file *vfs.FileDoc, lastVersion string, steps []Step) (*vfs.FileDoc, error) {
	lock := inst.NotesLock()
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	if len(steps) == 0 {
		return nil, ErrNoSteps
	}

	doc, err := get(inst, file)
	if err != nil {
		return nil, err
	}
	if lastVersion != fmt.Sprintf("%d", doc.Version) {
		return nil, ErrCannotApply
	}

	if t := timeToVersion(time.Now()); t > doc.Version {
		doc.Version = t
	}

	if err := apply(inst, doc, steps); err != nil {
		return nil, err
	}
	if err := saveSteps(inst, steps); err != nil {
		return nil, err
	}
	publishSteps(inst, file.ID(), steps)
	purgeOldSteps(inst, file.ID())

	if err := saveToCache(inst, doc); err != nil {
		return nil, err
	}
	return doc.asFile(file), nil
}

func apply(inst *instance.Instance, doc *Document, steps []Step) error {
	schema, err := doc.Schema()
	if err != nil {
		inst.Logger().WithField("nspace", "notes").
			Infof("Cannot instantiate the schema: %s", err)
		return ErrInvalidSchema
	}

	content, err := doc.Content()
	if err != nil {
		inst.Logger().WithField("nspace", "notes").
			Infof("Cannot instantiate the document: %s", err)
		return ErrInvalidFile
	}

	for i, s := range steps {
		step, err := transform.StepFromJSON(schema, s)
		if err != nil {
			inst.Logger().WithField("nspace", "notes").
				Infof("Cannot instantiate a step: %s", err)
			return ErrInvalidSteps
		}
		result := step.Apply(content)
		if result.Failed != "" {
			inst.Logger().WithField("nspace", "notes").
				Infof("Cannot apply a step: %s (version=%d)", result.Failed, doc.Version)
			return ErrCannotApply
		}
		content = result.Doc
		doc.Version++
		steps[i].SetID(stepID(doc.ID(), doc.Version))
		steps[i]["version"] = doc.Version
	}
	doc.SetContent(content)
	return nil
}

func saveSteps(inst *instance.Instance, steps []Step) error {
	olds := make([]interface{}, len(steps))
	news := make([]interface{}, len(steps))
	for i, s := range steps {
		news[i] = s
	}
	if err := couchdb.BulkUpdateDocs(inst, consts.NotesSteps, news, olds); err != nil {
		if !couchdb.IsNoDatabaseError(err) {
			return err
		}
		if err := couchdb.EnsureDBExist(inst, consts.NotesSteps); err != nil {
			return err
		}
		if err := couchdb.BulkUpdateDocs(inst, consts.NotesSteps, news, olds); err != nil {
			return err
		}
	}
	return nil
}

func purgeOldSteps(inst *instance.Instance, fileID string) {
	t := time.Now().Add(-cleanStepsAfter)
	id := stepID(fileID, timeToVersion(t))
	var steps []Step
	req := couchdb.AllDocsRequest{
		Limit:  1000,
		EndKey: id,
	}
	if err := couchdb.GetAllDocs(inst, consts.NotesSteps, &req, &steps); err != nil {
		inst.Logger().WithField("nspace", "notes").
			Warnf("Cannot purge old steps for file %s: %s", fileID, err)
		return
	}
	if len(steps) == 0 {
		return
	}
	docs := make([]couchdb.Doc, len(steps))
	for i := range steps {
		docs[i] = &steps[i]
	}
	if err := couchdb.BulkDeleteDocs(inst, consts.NotesSteps, docs); err != nil {
		inst.Logger().WithField("nspace", "notes").
			Warnf("Cannot purge old steps for file %s: %s", fileID, err)
	}
}

func publishSteps(inst *instance.Instance, fileID string, steps []Step) {
	for _, s := range steps {
		e := Event(s)
		e["doctype"] = s.DocType()
		e.SetID(fileID)
		e.publish(inst)
	}
}

var _ jsonapi.Object = &Step{}
