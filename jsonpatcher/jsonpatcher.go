package jsonpatcher

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	ds "github.com/ipfs/go-datastore"
	cbornode "github.com/ipfs/go-ipld-cbor"
	ipldformat "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log"
	"github.com/multiformats/go-multihash"
	core "github.com/textileio/go-textile-core/store"
)

type operationType int

const (
	create operationType = iota
	save
	delete
)

var (
	log                           = logging.Logger("jsonpatcher")
	errSavingNonExistentInstance  = errors.New("can't save nonexistent instance")
	errCantCreateExistingInstance = errors.New("cant't create already existent instance")
	errUnknownOperation           = errors.New("unknown operation type")
)

type operation struct {
	Type      operationType
	EntityID  core.EntityID
	JSONPatch []byte
}

type jsonPatcher struct {
	jsonMode bool
}

var _ core.EventCodec = (*jsonPatcher)(nil)

func init() {
	cbornode.RegisterCborType(patchEvent{})
	cbornode.RegisterCborType(recordEvents{})
	cbornode.RegisterCborType(operation{})
	cbornode.RegisterCborType(time.Time{})
}

// New returns a JSON-Patcher EventCodec
func New(jsonMode bool) core.EventCodec {
	return &jsonPatcher{jsonMode: jsonMode}
}

func (jp *jsonPatcher) Create(actions []core.Action) ([]core.Event, ipldformat.Node, error) {
	revents := recordEvents{Patches: make([]patchEvent, len(actions))}
	events := make([]core.Event, len(actions))
	for i := range actions {
		var op *operation
		var err error
		switch actions[i].Type {
		case core.Create:
			op, err = createEvent(actions[i].EntityID, actions[i].Current, jp.jsonMode)
		case core.Save:
			op, err = saveEvent(actions[i].EntityID, actions[i].Previous, actions[i].Current, jp.jsonMode)
		case core.Delete:
			op, err = deleteEvent(actions[i].EntityID)
		default:
			panic("unkown action type")
		}
		if err != nil {
			return nil, nil, err
		}
		revents.Patches[i] = patchEvent{
			Timestamp: time.Now(),
			ID:        actions[i].EntityID,
			ModelName: actions[i].ModelName,
			Patch:     *op,
		}
		events[i] = revents.Patches[i]
	}

	n, err := cbornode.WrapObject(revents, multihash.SHA2_256, -1)
	if err != nil {
		return nil, nil, err
	}
	return events, n, nil
}

func (jp *jsonPatcher) Reduce(events []core.Event, datastore ds.TxnDatastore, baseKey ds.Key) ([]core.ReduceAction, error) {
	txn, err := datastore.NewTransaction(false)
	if err != nil {
		return nil, err
	}
	defer txn.Discard()

	actions := make([]core.ReduceAction, len(events))
	for i, e := range events {
		je, ok := e.(patchEvent)
		if !ok {
			return nil, fmt.Errorf("event unrecognized for jsonpatcher eventcodec")
		}
		key := baseKey.ChildString(e.Model()).ChildString(e.EntityID().String())
		switch je.Patch.Type {
		case create:
			exist, err := txn.Has(key)
			if err != nil {
				return nil, err
			}
			if exist {
				return nil, errCantCreateExistingInstance
			}
			if err := txn.Put(key, je.Patch.JSONPatch); err != nil {
				return nil, fmt.Errorf("error when reducing create event: %v", err)
			}
			actions[i] = core.ReduceAction{Type: core.Create, Model: e.Model(), EntityID: e.EntityID()}
			log.Debug("\tcreate operation applied")
		case save:
			value, err := txn.Get(key)
			if errors.Is(err, ds.ErrNotFound) {
				return nil, errSavingNonExistentInstance
			}
			if err != nil {
				return nil, err
			}
			patchedValue, err := jsonpatch.MergePatch(value, je.Patch.JSONPatch)
			if err != nil {
				return nil, fmt.Errorf("error when reducing save event: %v", err)
			}
			if err = txn.Put(key, patchedValue); err != nil {
				return nil, err
			}
			actions[i] = core.ReduceAction{Type: core.Save, Model: e.Model(), EntityID: e.EntityID()}
			log.Debug("\tsave operation applied")
		case delete:
			if err := txn.Delete(key); err != nil {
				return nil, err
			}
			actions[i] = core.ReduceAction{Type: core.Delete, Model: e.Model(), EntityID: e.EntityID()}
			log.Debug("\tdelete operation applied")
		default:
			return nil, errUnknownOperation
		}
	}
	if err := txn.Commit(); err != nil {
		return nil, err
	}

	return actions, nil
}

type recordEvents struct {
	Patches []patchEvent
}

// EventsFromBytes returns a unmarshaled event from its bytes representation
func (jp *jsonPatcher) EventsFromBytes(data []byte) ([]core.Event, error) {
	revents := recordEvents{}
	if err := cbornode.DecodeInto(data, &revents); err != nil {
		return nil, err
	}

	res := make([]core.Event, len(revents.Patches))
	for i := range revents.Patches {
		res[i] = revents.Patches[i]
	}

	return res, nil
}

func createEvent(id core.EntityID, v interface{}, jsonMode bool) (*operation, error) {
	var opBytes []byte

	if jsonMode {
		strjson := v.(*string)
		opBytes = []byte(*strjson)
	} else {
		var err error
		opBytes, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	}
	return &operation{
		Type:      create,
		EntityID:  id,
		JSONPatch: opBytes,
	}, nil
}

func saveEvent(id core.EntityID, prev interface{}, curr interface{}, jsonMode bool) (*operation, error) {
	var prevBytes, currBytes []byte
	if jsonMode {
		strCurrJson := curr.(*string)

		prevBytes = prev.([]byte)
		currBytes = []byte(*strCurrJson)
	} else {
		var err error
		prevBytes, err = json.Marshal(prev)
		if err != nil {
			return nil, err
		}
		currBytes, err = json.Marshal(curr)
		if err != nil {
			return nil, err
		}
	}
	jsonPatch, err := jsonpatch.CreateMergePatch(prevBytes, currBytes)
	if err != nil {
		return nil, err
	}
	return &operation{
		Type:      save,
		EntityID:  id,
		JSONPatch: jsonPatch,
	}, nil
}

func deleteEvent(id core.EntityID) (*operation, error) {
	return &operation{
		Type:      delete,
		EntityID:  id,
		JSONPatch: nil,
	}, nil
}

type patchEvent struct {
	Timestamp time.Time
	ID        core.EntityID
	ModelName string
	Patch     operation
}

func (je patchEvent) Time() []byte {
	t := je.Timestamp.UnixNano()
	buf := new(bytes.Buffer)
	// Use big endian to preserve lexicographic sorting
	_ = binary.Write(buf, binary.BigEndian, t)
	return buf.Bytes()
}

func (je patchEvent) EntityID() core.EntityID {
	return je.ID
}

func (je patchEvent) Model() string {
	return je.ModelName
}

var _ core.Event = (*patchEvent)(nil)
