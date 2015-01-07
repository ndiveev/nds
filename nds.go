package nds

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"math/rand"
	"reflect"
	"time"

	"appengine"

	"appengine/datastore"
	"appengine/memcache"
)

const (
	// memcachePrefix is the namespace memcache uses to store entities.
	memcachePrefix = "NDS1:"

	// memcacheLockTime is the maximum length of time a memcache lock will be
	// held for. 32 seconds is choosen as 30 seconds is the maximum amount of
	// time an underlying datastore call will retry even if the API reports a
	// success to the user.
	memcacheLockTime = 32 * time.Second

	// memcacheMaxKeySize is the maximum size a memcache item key can be. Keys
	// greater than this size are automatically hashed to a smaller size.
	memcacheMaxKeySize = 250
)

var (
	typeOfPropertyLoadSaver = reflect.TypeOf(
		(*datastore.PropertyLoadSaver)(nil)).Elem()
	typeOfPropertyList = reflect.TypeOf(datastore.PropertyList(nil))
)

// The variables in this block are here so that we can test all error code
// paths by substituting the respective functions with error producing ones.
var (
	datastoreDeleteMulti = datastore.DeleteMulti
	datastoreGetMulti    = datastore.GetMulti
	datastorePutMulti    = datastore.PutMulti

	// Memcache calls are replaced with ones that don't hit the backend service
	// if len(keys) or len(items) == 0. This should be changed once issue
	// http://goo.gl/AW96Fi has been resolved with the Go App Engine SDK.
	memcacheAddMulti            = zeroMemcacheAddMulti
	memcacheCompareAndSwapMulti = zeroMemcacheCompareAndSwapMulti
	memcacheDeleteMulti         = zeroMemcacheDeleteMulti
	memcacheGetMulti            = zeroMemcacheGetMulti
	memcacheSetMulti            = zeroMemcacheSetMulti

	marshal   = marshalPropertyList
	unmarshal = unmarshalPropertyList
)

// The following memcache functions are enclosed to ensure the underlying
// App Engine service is not called when there are no keys or items to be
// called with. The datastore calls do not need this because they already check
// for this condition and short-circuit.
func zeroMemcacheAddMulti(c appengine.Context, items []*memcache.Item) error {
	if len(items) == 0 {
		return nil
	}
	return memcache.AddMulti(c, items)
}

func zeroMemcacheCompareAndSwapMulti(c appengine.Context,
	items []*memcache.Item) error {
	if len(items) == 0 {
		return nil
	}
	return memcache.CompareAndSwapMulti(c, items)
}

func zeroMemcacheGetMulti(c appengine.Context, keys []string) (
	map[string]*memcache.Item, error) {
	if len(keys) == 0 {
		return make(map[string]*memcache.Item, 0), nil
	}
	return memcache.GetMulti(c, keys)
}

func zeroMemcacheDeleteMulti(c appengine.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return memcache.DeleteMulti(c, keys)
}

func zeroMemcacheSetMulti(c appengine.Context, items []*memcache.Item) error {
	if len(items) == 0 {
		return nil
	}
	return memcache.SetMulti(c, items)
}

const (
	noneItem uint32 = iota
	entityItem
	lockItem
)

func init() {
	gob.Register(time.Time{})
	gob.Register(datastore.ByteString{})
	gob.Register(&datastore.Key{})
	gob.Register(appengine.BlobKey(""))
	gob.Register(appengine.GeoPoint{})
}

func itemLock() []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, rand.Uint32())
	return b
}

func checkMultiArgs(keys []*datastore.Key, v reflect.Value) error {
	if v.Kind() != reflect.Slice {
		return errors.New("nds: vals is not a slice")
	}

	if len(keys) != v.Len() {
		return errors.New("nds: keys and vals slices have different length")
	}

	isNilErr, nilErr := false, make(appengine.MultiError, len(keys))
	for i, key := range keys {
		if key == nil {
			isNilErr = true
			nilErr[i] = datastore.ErrInvalidKey
		}
	}
	if isNilErr {
		return nilErr
	}

	if v.Type() == typeOfPropertyList {
		return errors.New("nds: PropertyList not supported")
	}

	elemType := v.Type().Elem()
	if reflect.PtrTo(elemType).Implements(typeOfPropertyLoadSaver) {
		return nil
	}

	switch elemType.Kind() {
	case reflect.Struct, reflect.Interface:
		return nil
	case reflect.Ptr:
		elemType = elemType.Elem()
		if elemType.Kind() == reflect.Struct {
			return nil
		}
	}
	return errors.New("nds: unsupported vals type")
}

func createMemcacheKey(key *datastore.Key) string {
	memcacheKey := memcachePrefix + key.Encode()
	if len(memcacheKey) > memcacheMaxKeySize {
		hash := sha1.Sum([]byte(memcacheKey))
		memcacheKey = hex.EncodeToString(hash[:])
	}
	return memcacheKey
}

// SaveStruct saves src to a datastore.PropertyList. src must be a struct
// pointer.
func SaveStruct(src interface{}, pl *datastore.PropertyList) error {
	c, err := make(chan datastore.Property), make(chan error)
	go func() {
		err <- datastore.SaveStruct(src, c)
	}()
	for p := range c {
		*pl = append(*pl, p)
	}
	return <-err
}

// LoadStruct loads a datastore.PropertyList into dst. dst must be a struct
// pointer.
func LoadStruct(dst interface{}, pl datastore.PropertyList) error {
	c := make(chan datastore.Property)
	go func() {
		for _, p := range pl {
			c <- p
		}
		close(c)
	}()
	return datastore.LoadStruct(dst, c)
}

func propertyLoadSaverToPropertyList(
	pls datastore.PropertyLoadSaver, pl *datastore.PropertyList) error {
	c, err := make(chan datastore.Property), make(chan error)
	go func() {
		err <- pls.Save(c)
	}()
	for p := range c {
		*pl = append(*pl, p)
	}
	return <-err
}

func propertyListToPropertyLoadSaver(
	pl datastore.PropertyList, pls datastore.PropertyLoadSaver) error {

	c := make(chan datastore.Property)
	go func() {
		for _, p := range pl {
			c <- p
		}
		close(c)
	}()

	return pls.Load(c)
}

func marshalPropertyList(pl datastore.PropertyList) ([]byte, error) {
	buf := bytes.Buffer{}
	if err := gob.NewEncoder(&buf).Encode(&pl); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unmarshalPropertyList(data []byte, pl *datastore.PropertyList) error {
	return gob.NewDecoder(bytes.NewBuffer(data)).Decode(pl)
}

func setValue(val reflect.Value, pl datastore.PropertyList) error {

	if reflect.PtrTo(val.Type()).Implements(typeOfPropertyLoadSaver) {
		val = val.Addr()
	}

	if pls, ok := val.Interface().(datastore.PropertyLoadSaver); ok {
		return propertyListToPropertyLoadSaver(pl, pls)
	}

	if val.Kind() == reflect.Struct {
		val = val.Addr()
	}
	return LoadStruct(val.Interface(), pl)
}
