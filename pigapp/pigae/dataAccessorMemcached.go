package pigae

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"time"

	. "github.com/Deleplace/programming-idioms/pig"

	"appengine"
	"appengine/datastore"
	"appengine/memcache"
)

// This source file has a lot of duplicated code : "if cached then return else datastore and cache".
// TODO: find a smarter design for this "proxy" type which applies basically the same behavior to
// all read methods, and the same behavior to all write methods.

// MemcacheDatastoreAccessor accessor uses a MemCache for standard CRUD.
//
// Some methods are not redefined : randomIdiom, nextIdiomID, nextImplID, processUploadFile, processUploadFiles
type MemcacheDatastoreAccessor struct {
	dataAccessor
}

func (a *MemcacheDatastoreAccessor) cacheValue(c appengine.Context, cacheKey string, data interface{}, expiration time.Duration) error {
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)

	err := enc.Encode(&data)
	if err != nil {
		c.Debugf("Failed encoding for cache[%v] : %v\n", cacheKey, err)
		return err
	}
	cacheItem := &memcache.Item{
		Key:        cacheKey,
		Value:      buffer.Bytes(),
		Expiration: expiration,
	}
	// Set the item, unconditionally
	err = memcache.Set(c, cacheItem)
	if err != nil {
		c.Debugf("Failed setting cache[%v] : %v\n", cacheKey, err)
	} else {
		// c.Debugf("Successfully set cache[%v]\n", cacheKey)
	}
	return err
}

// Just a shortcut for caching the datastoreKey + value
func (a *MemcacheDatastoreAccessor) cacheKeyValue(c appengine.Context, cacheKey string, datastoreKey *datastore.Key, entity interface{}, expiration time.Duration) error {
	kae := &KeyAndEntity{datastoreKey, entity}
	return a.cacheValue(c, cacheKey, kae, expiration)
}

// Just a shortcut for caching the pair
func (a *MemcacheDatastoreAccessor) cachePair(c appengine.Context, cacheKey string, first interface{}, second interface{}, expiration time.Duration) error {
	pair := &pair{first, second}
	return a.cacheValue(c, cacheKey, pair, expiration)
}

func (a *MemcacheDatastoreAccessor) readCache(c appengine.Context, cacheKey string) (interface{}, error) {
	// Get the item from the memcache
	var cacheItem *memcache.Item
	var err error
	if cacheItem, err = memcache.Get(c, cacheKey); err == memcache.ErrCacheMiss {
		// Item not in the cache
		return nil, nil
	} else if err != nil {
		return nil, err
	} else {
		buffer := bytes.NewBuffer(cacheItem.Value) // todo avoid bytes copy ??
		dec := gob.NewDecoder(buffer)
		var data interface{}
		err = dec.Decode(&data)
		return data, err
	}
}

func init() {
	gob.Register(&KeyAndEntity{})
	gob.Register(&pair{})
	gob.Register(&Idiom{})
	gob.Register([]*datastore.Key{})
	gob.Register([]*Idiom{})
	gob.Register([]string{})
	gob.Register(map[string]bool{})
	gob.Register(Toggles{})
	gob.Register(&ApplicationConfig{})
}

// KeyAndEntity is a specific pair wrapper.
type KeyAndEntity struct {
	Key    *datastore.Key
	Entity interface{}
}

type pair struct {
	First  interface{}
	Second interface{}
}

func (a *MemcacheDatastoreAccessor) recacheIdiom(c appengine.Context, key *datastore.Key, idiom *Idiom) error {
	cacheKey := fmt.Sprintf("getIdiom(%v)", idiom.Id)
	err := a.cacheKeyValue(c, cacheKey, key, idiom, 24*time.Hour)
	if err != nil {
		c.Errorf(err.Error())
		return err
	}

	for _, impl := range idiom.Implementations {
		cacheKey = fmt.Sprintf("getIdiomByImplID(%v)", impl.Id)
		err = a.cacheKeyValue(c, cacheKey, key, idiom, 24*time.Hour)
		if err != nil {
			c.Errorf(err.Error())
			return err
		}
	}
	// Unfortunatly, some previous "getIdiomByImplID(xyz)" might be left uninvalidated.
	// (theoretically)
	return err
}

func (a *MemcacheDatastoreAccessor) uncacheIdiom(c appengine.Context, idiom *Idiom) error {
	cacheKeys := make([]string, 1+len(idiom.Implementations))
	cacheKeys[0] = fmt.Sprintf("getIdiom(%v)", idiom.Id)
	for i, impl := range idiom.Implementations {
		cacheKeys[1+i] = fmt.Sprintf("getIdiomByImplID(%v)", impl.Id)
	}

	err := memcache.DeleteMulti(c, cacheKeys)
	if err != nil {
		c.Errorf(err.Error())
	}
	return err
}

func (a *MemcacheDatastoreAccessor) getIdiom(c appengine.Context, idiomID int) (*datastore.Key, *Idiom, error) {
	cacheKey := fmt.Sprintf("getIdiom(%v)", idiomID)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.getIdiom(c, idiomID)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		key, idiom, err := a.dataAccessor.getIdiom(c, idiomID)
		if err == nil {
			a.recacheIdiom(c, key, idiom)
		}
		return key, idiom, err
	}
	// Found in cache :)
	kae := data.(*KeyAndEntity)
	key := kae.Key
	idiom := kae.Entity.(*Idiom)
	return key, idiom, nil
}

func (a *MemcacheDatastoreAccessor) getIdiomByImplID(c appengine.Context, implID int) (*datastore.Key, *Idiom, error) {
	cacheKey := fmt.Sprintf("getIdiomByImplID(%v)", implID)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.getIdiomByImplID(c, implID)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		key, idiom, err := a.dataAccessor.getIdiomByImplID(c, implID)
		if err == nil {
			a.cacheKeyValue(c, cacheKey, key, idiom, 24*time.Hour)
		}
		return key, idiom, err
	}
	// Found in cache :)
	kae := data.(*KeyAndEntity)
	key := kae.Key
	idiom := kae.Entity.(*Idiom)
	return key, idiom, nil
}

func (a *MemcacheDatastoreAccessor) saveNewIdiom(c appengine.Context, idiom *Idiom) (*datastore.Key, error) {
	key, err := a.dataAccessor.saveNewIdiom(c, idiom)
	if err == nil {
		a.recacheIdiom(c, key, idiom)
	}
	return key, err
}

func (a *MemcacheDatastoreAccessor) saveExistingIdiom(c appengine.Context, key *datastore.Key, idiom *Idiom) error {
	c.Debugf("Saving idiom %v\n", idiom.Id)
	err := a.dataAccessor.saveExistingIdiom(c, key, idiom)
	if err == nil {
		a.recacheIdiom(c, key, idiom)
	}
	return err
}

func (a *MemcacheDatastoreAccessor) getAllIdioms(c appengine.Context, limit int, order string) ([]*datastore.Key, []*Idiom, error) {
	cacheKey := fmt.Sprintf("getAllIdioms(%v,%v)", limit, order)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.getAllIdioms(c, limit, order)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		keys, idioms, err := a.dataAccessor.getAllIdioms(c, limit, order)
		if err == nil {
			// Cached "All idioms" will have a 10mn lag after an idiom/impl creation.
			//a.cachePair(c, cacheKey, keys, idioms, 10*time.Minute)
			// For now, it might mange too often
			a.cachePair(c, cacheKey, keys, idioms, 30*time.Second)
		}
		return keys, idioms, err
	}
	// Found in cache :)
	pair := data.(*pair)
	keys := pair.First.([]*datastore.Key)
	idioms := pair.Second.([]*Idiom)
	return keys, idioms, nil
}

func (a *MemcacheDatastoreAccessor) deleteAllIdioms(c appengine.Context) error {
	err := a.dataAccessor.deleteAllIdioms(c)
	// Cache : the nuclear option!
	memcache.Flush(c)
	return err
}

func (a *MemcacheDatastoreAccessor) deleteIdiom(c appengine.Context, idiomID int) error {
	// Clear cache entries
	_, idiom, err := a.dataAccessor.getIdiom(c, idiomID)
	if err == nil {
		a.uncacheIdiom(c, idiom)
	}

	// Delete in datastore
	return a.dataAccessor.deleteIdiom(c, idiomID)
}

func (a *MemcacheDatastoreAccessor) deleteImpl(c appengine.Context, idiomID int, implID int) error {
	// Clear cache entries
	_, idiom, err := a.dataAccessor.getIdiom(c, idiomID)
	if err == nil {
		a.uncacheIdiom(c, idiom)
	}

	err = a.dataAccessor.deleteImpl(c, idiomID, implID)
	return err
}

// Language filter lang is optional.
func (a *MemcacheDatastoreAccessor) searchIdiomsByWords(c appengine.Context, words []string, lang string, limit int) ([]*Idiom, error) {
	cacheKey := fmt.Sprintf("searchIdiomsByWords(%v,%v,%v)", words, lang, limit)
	//c.Debugf(cacheKey)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.searchIdiomsByWords(c, words, lang, limit)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		idioms, err := a.dataAccessor.searchIdiomsByWords(c, words, lang, limit)
		if err == nil {
			// Search results will have a 10mn lag after an idiom/impl creation/update.
			a.cacheValue(c, cacheKey, idioms, 10*time.Minute)
		}
		return idioms, err
	}
	// Found in cache :)
	idioms := data.([]*Idiom)
	return idioms, nil
}

func (a *MemcacheDatastoreAccessor) searchIdiomsByWordsWithFavorites(c appengine.Context, words []string, favoriteLangs []string, seeNonFavorite bool, limit int) ([]*Idiom, error) {
	if len(favoriteLangs) == 0 {
		// Searches without preferences are cached
		return a.dataAccessor.searchIdiomsByWords(c, words, "", limit)
	}
	// Personalized searches not cached (yet)
	return a.dataAccessor.searchIdiomsByWordsWithFavorites(c, words, favoriteLangs, seeNonFavorite, limit)
}

func (a *MemcacheDatastoreAccessor) searchIdiomsByLangs(c appengine.Context, langs []string, limit int) ([]*Idiom, error) {
	cacheKey := fmt.Sprintf("searchIdiomsByLangs(%v,%v)", langs, limit)
	//c.Debugf(cacheKey)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.searchIdiomsByLangs(c, langs, limit)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		idioms, err := a.dataAccessor.searchIdiomsByLangs(c, langs, limit)
		if err == nil {
			// Search results will have a 10mn lag after an idiom/impl creation/update.
			a.cacheValue(c, cacheKey, idioms, 10*time.Minute)
		}
		return idioms, err
	}
	// Found in cache :)
	idioms := data.([]*Idiom)
	return idioms, nil
}

func (a *MemcacheDatastoreAccessor) languagesHavingImpl(c appengine.Context) (langs []string) {
	cacheKey := "languagesHavingImpl()"
	//c.Debugf("Getting cache[%v]\n", cacheKey)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.languagesHavingImpl(c)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		langs = a.dataAccessor.languagesHavingImpl(c)
		//dataAccessor.cacheValue(c, cacheKey, langs, 24*time.Hour)
		// For now, it might mange too often
		a.cacheValue(c, cacheKey, langs, 5*time.Minute)
		return
	}
	// Found in cache :)
	langs = data.([]string)
	return
}

func (a *MemcacheDatastoreAccessor) recentIdioms(c appengine.Context, favoriteLangs []string, showOther bool, n int) ([]*Idiom, error) {
	cacheKey := fmt.Sprintf("recentIdioms(%v,%v,%v)", favoriteLangs, showOther, n)
	//c.Debugf(cacheKey)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.recentIdioms(c, favoriteLangs, showOther, n)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		idioms, err := a.dataAccessor.recentIdioms(c, favoriteLangs, showOther, n)
		if err == nil {
			// "Popular idioms" will have a 10mn lag after an idiom/impl creation.
			a.cacheValue(c, cacheKey, idioms, 10*time.Minute)
		}
		return idioms, err
	}
	// Found in cache :)
	idioms := data.([]*Idiom)
	return idioms, nil
}

func (a *MemcacheDatastoreAccessor) popularIdioms(c appengine.Context, favoriteLangs []string, showOther bool, n int) ([]*Idiom, error) {
	cacheKey := fmt.Sprintf("popularIdioms(%v,%v,%v)", favoriteLangs, showOther, n)
	//c.Debugf(cacheKey)
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.popularIdioms(c, favoriteLangs, showOther, n)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		idioms, err := a.dataAccessor.popularIdioms(c, favoriteLangs, showOther, n)
		if err == nil {
			// "Popular idioms" will have a 10mn lag after an idiom/impl creation.
			a.cacheValue(c, cacheKey, idioms, 10*time.Minute)
		}
		return idioms, err
	}
	// Found in cache :)
	idioms := data.([]*Idiom)
	return idioms, nil
}

// TODO cache this
func (a *MemcacheDatastoreAccessor) idiomsFilterOrder(c appengine.Context, favoriteLangs []string, limitEachLang int, showOther bool, sortOrder string) ([]*Idiom, error) {
	idioms, err := a.dataAccessor.idiomsFilterOrder(c, favoriteLangs, limitEachLang, showOther, sortOrder)
	return idioms, err
}

func (a *MemcacheDatastoreAccessor) getAppConfig(c appengine.Context) (ApplicationConfig, error) {
	cacheKey := "getAppConfig()"
	data, cacheerr := a.readCache(c, cacheKey)
	if cacheerr != nil {
		c.Errorf(cacheerr.Error())
		// Ouch. Well, skip the cache if it's broken
		return a.dataAccessor.getAppConfig(c)
	}
	if data == nil {
		// Not in the cache. Then fetch the real datastore data. And cache it.
		appConfig, err := a.dataAccessor.getAppConfig(c)
		if err == nil {
			a.cacheValue(c, cacheKey, appConfig, 24*time.Hour)
		} else {
			c.Infof("Retrieved ApplicationConfig (Toggles) from Datastore\n")
		}
		return appConfig, err
	}
	// Found in cache :)
	appConfig := data.(*ApplicationConfig)
	return *appConfig, nil
}

func (a *MemcacheDatastoreAccessor) saveAppConfig(c appengine.Context, appConfig ApplicationConfig) error {
	memcache.Flush(c)
	return a.dataAccessor.saveAppConfig(c, appConfig)
	// TODO force toggles refresh for all instances, after memcache flush
}

func (a *MemcacheDatastoreAccessor) deleteCache(c appengine.Context) error {
	return memcache.Flush(c)
}