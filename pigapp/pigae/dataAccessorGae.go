package pigae

import (
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	. "github.com/Deleplace/programming-idioms/pig"

	"appengine"
	"appengine/blobstore"
	"appengine/datastore"
	"appengine/delay"
	gaesearch "appengine/search"
)

// GaeDatastoreAccessor is a dataAccessor that works on the Google App Engine Datastore
type GaeDatastoreAccessor struct {
}

// searchableDoc is the searchable unit for 1 idiom.
// We keep only some references (id and key) in the indexed "documents", not
// the whole idioms+implementations data.
// Actually, by choosing a Key string as docID we don't need to retrieve
// the searchableDoc contents when searching, because Keys suffice.
// See https://cloud.google.com/appengine/docs/go/search/
type searchableDoc struct {
	IdiomKeyString gaesearch.Atom
	IdiomId        gaesearch.Atom
	// Bulk is a simple concatenation of (normalized) words, space-separated
	Bulk string
	// Langs is a concatenation of implemented languages, for filtering
	Langs string

	// + displayable data for result list?
	//idiomTitle
	//implLanguages
	//implIDs
	//...
}

func (a *GaeDatastoreAccessor) getIdiom(c appengine.Context, idiomID int) (*datastore.Key, *Idiom, error) {
	var idiom Idiom
	key := datastore.NewKey(c, "Idiom", "", int64(idiomID), nil)
	err := datastore.Get(c, key, &idiom)
	return key, &idiom, err
}

func (a *GaeDatastoreAccessor) getIdiomByImplID(c appengine.Context, implID int) (*datastore.Key, *Idiom, error) {
	q := datastore.NewQuery("Idiom").Filter("Implementations.Id =", implID)
	idioms := make([]*Idiom, 0, 1)
	keys, err := q.GetAll(c, &idioms)
	if err != nil {
		return nil, nil, err
	}
	if len(idioms) < 1 {
		err = fmt.Errorf("Idiom with implementation id %d not found.", implID)
		return nil, nil, err
	}
	if len(idioms) > 1 {
		err = fmt.Errorf("Multiple Idioms match implementation id %d !", implID)
		return nil, nil, err
	}
	return keys[0], idioms[0], nil
}

// Delayers registered at init time

var historyDelayer = delay.Func("save-history-item", func(c appengine.Context, idiomKey *datastore.Key) error {
	var historyItem IdiomHistory
	// TODO check Memcache first
	err := datastore.Get(c, idiomKey, &historyItem.Idiom)
	if err != nil {
		return err
	}
	// Saves a new IdiomHistory entity. This causes no contention on the original Idiom entity.
	_, err = datastore.Put(c, newHistoryKey(c), &historyItem)
	return err
})

var indexDelayer = delay.Func("index-text-idiom", func(c appengine.Context, idiomKey *datastore.Key) error {
	var idiom Idiom
	// TODO check Memcache first
	err := datastore.Get(c, idiomKey, &idiom)
	if err != nil {
		return err
	}
	// Full text API causes no contention on the original Idiom entity.
	err = indexIdiomFullText(c, &idiom, idiomKey)
	return err
})

func (a *GaeDatastoreAccessor) saveNewIdiom(c appengine.Context, idiom *Idiom) (*datastore.Key, error) {
	now := time.Now()
	idiom.CreationDate = now
	idiom.Version = 1
	idiom.VersionDate = now
	idiom.ImplCount = len(idiom.Implementations)
	key, err := datastore.Put(c, datastore.NewKey(c, "Idiom", "", int64(idiom.Id), nil), idiom)
	if err != nil {
		return key, err
	}

	// Index full-text : asynchronously
	indexDelayer.Call(c, key)

	// Save an history item : asynchronously
	historyDelayer.Call(c, key)

	return key, err
}

func (a *GaeDatastoreAccessor) saveExistingIdiom(c appengine.Context, key *datastore.Key, idiom *Idiom) error {
	idiom.Version = idiom.Version + 1
	idiom.VersionDate = time.Now()
	idiom.ImplCount = len(idiom.Implementations)
	_, err := datastore.Put(c, key, idiom)

	// Index full-text : asynchronously
	indexDelayer.Call(c, key)

	// Save an history item : asynchronously
	historyDelayer.Call(c, key)

	return err
}

func newHistoryKey(c appengine.Context) *datastore.Key {
	return datastore.NewIncompleteKey(c, "IdiomHistory", nil)
}

func indexIdiomFullText(c appengine.Context, idiom *Idiom, idiomKey *datastore.Key) error {
	index, err := gaesearch.Open("idioms")
	if err != nil {
		return err
	}
	// By using directly the idiom Key as docID,
	// we can leverage faster ID-only search later.
	docID := idiomKey.Encode()
	w, _ := idiom.ExtractIndexableWords()
	doc := &searchableDoc{
		IdiomKeyString: gaesearch.Atom(idiomKey.Encode()),
		IdiomId:        gaesearch.Atom(strconv.Itoa(idiom.Id)),
		Bulk:           strings.Join(w, " "),
		Langs:          implementedLanguagesConcat(idiom),
	}
	_, err = index.Put(c, docID, doc)
	return err
}

// implementedLanguages may return duplicates, which is ok
func implementedLanguagesConcat(idiom *Idiom) string {
	langs := ""
	for _, impl := range idiom.Implementations {
		langs += impl.LanguageName + " "
	}
	// TODO add non-canonical synonyms...
	return langs
}

func (a *GaeDatastoreAccessor) getAllIdioms(c appengine.Context, limit int, order string) ([]*datastore.Key, []*Idiom, error) {
	q := datastore.NewQuery("Idiom")
	if order != "" {
		q = q.Order(order)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	idioms := make([]*Idiom, 0, 40)
	//idioms = []*Idiom{}
	keys, err := q.GetAll(c, &idioms)
	if err != nil {
		return nil, nil, err
	}
	return keys, idioms, nil
}

func (a *GaeDatastoreAccessor) deleteAllIdioms(c appengine.Context) error {
	keys, err := datastore.NewQuery("Idiom").KeysOnly().GetAll(c, nil)
	if err != nil {
		return err
	}

	// Must remove 1 by 1 (Index has no batch methods)
	index, err := gaesearch.Open("idioms")
	if err != nil {
		return err
	}
	for _, key := range keys {
		docID := key.Encode()
		index.Delete(c, docID)
		if err != nil {
			return err
		}
	}

	return datastore.DeleteMulti(c, keys)
}

func (a *GaeDatastoreAccessor) deleteIdiom(c appengine.Context, idiomID int) error {
	key, _, err := a.getIdiom(c, idiomID)
	if err != nil {
		return err
	}
	return datastore.Delete(c, key)
}

func (a *GaeDatastoreAccessor) deleteImpl(c appengine.Context, idiomID int, implID int) error {
	key, idiom, err := a.getIdiom(c, idiomID)
	if err != nil {
		return err
	}
	if i, _, found := idiom.FindImplInIdiom(implID); found {
		idiom.Implementations = append(idiom.Implementations[:i], idiom.Implementations[i+1:]...)
		return a.saveExistingIdiom(c, key, idiom)
	}
	return fmt.Errorf("Could not find impl %v in idiom %v", idiom.Id, implID)
}

// Language filter lang is optional.
// DEPRECATED: this method should not be useful anymore.
func (a *GaeDatastoreAccessor) searchIdiomsByWords(c appengine.Context, words []string, lang string, limit int) ([]*Idiom, error) {
	if lang == "" {
		return a.searchIdiomsByWordsWithFavorites(c, words, nil, true, limit)
	}
	return a.searchIdiomsByWordsWithFavorites(c, words, []string{lang}, false, limit)
}

// searchIdiomsByWordsWithFavorites must return idioms that contain *all* the searched words.
// If seeNonFavorite==false, it must only return idioms that have at least 1 implementation in 1 of the user favoriteLangs.
// If seeNonFavorite==true, it must return the same list but extended with idioms that contain all the searched words but no implementation in a user favoriteLang.
func (a *GaeDatastoreAccessor) searchIdiomsByWordsWithFavorites(c appengine.Context, words []string, favoriteLangs []string, seeNonFavorite bool, limit int) ([]*Idiom, error) {
	// "~" is the stemming prefix, so "~dog" matches "dogs".
	baseQuery := "Bulk:(~" + strings.Join(words, " AND ~") + ")"
	if len(favoriteLangs) == 0 {
		// No favlangs
		c.Infof("Full text query: %v ", baseQuery)
		return executeIdiomTextSearchQuery(c, baseQuery, limit)
	}
	langsClause := "Langs:(" + strings.Join(favoriteLangs, " OR ") + ")"
	queryFav := baseQuery + " AND " + langsClause
	// queryFav looks like "Bulk: string integer AND Langs:(Java OR Go OR Python)"
	part1, err := executeIdiomTextSearchQuery(c, queryFav, limit)
	if err != nil || len(part1) >= limit || !seeNonFavorite {
		return part1, err
	}

	queryNonFav := baseQuery + " AND NOT " + langsClause
	// queryNonFav looks like "Bulk: string integer AND NOT Langs:(Java OR Go OR Python)"
	part2, err := executeIdiomTextSearchQuery(c, queryNonFav, limit-len(part1))
	if err != nil {
		// Return the most important partial result: part1
		return part1, err
	}
	idioms := append(part1, part2...)
	return idioms, nil
}

func executeIdiomTextSearchQuery(c appengine.Context, query string, limit int) ([]*Idiom, error) {
	// c.Infof(query)
	index, err := gaesearch.Open("idioms")
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		// Limit is not optional. 0 means zero result.
		return nil, nil
	}
	idiomKeys := make([]*datastore.Key, 0, limit)
	// This is an *IDsOnly* search, where docID == Key
	it := index.Search(c, query, &gaesearch.SearchOptions{
		Limit:   limit,
		IDsOnly: true,
	})
	for {
		idiomKeyString, err := it.Next(nil)
		if err == gaesearch.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		key, err := datastore.DecodeKey(idiomKeyString)
		if err != nil {
			return nil, err
		}
		idiomKeys = append(idiomKeys, key)
	}
	// Fetch Idioms in a []Idiom
	buffer := make([]Idiom, len(idiomKeys))
	err = datastore.GetMulti(c, idiomKeys, buffer)
	// Convert []Idiom to []*Idiom
	idioms := make([]*Idiom, len(buffer))
	for i := range buffer {
		// Do not take the address of the 2nd range variable, it would make a copy.
		// Better take the address in the existing buffer.
		idioms[i] = &buffer[i]
	}
	return idioms, err
}

func (a *GaeDatastoreAccessor) searchIdiomsByLangs(c appengine.Context, langs []string, limit int) ([]*Idiom, error) {
	dsq := datastore.NewQuery("Idiom")
	dsq = dsq.Filter("Implementations.LanguageName = ", langs[0])
	if len(langs) >= 2 {
		return nil, fmt.Errorf("Not yet implemented: list for more than 1 language")
	}
	dsq = dsq.Order("-Rating").Limit(limit)
	hits := make([]*Idiom, 0, 10)
	if _, err := dsq.GetAll(c, &hits); err != nil {
		return nil, err
	}
	return hits, nil
}

func (a *GaeDatastoreAccessor) processUploadFile(r *http.Request, name string) (string, map[string][]string, error) {
	// See https://developers.google.com/appengine/docs/go/blobstore/#Uploading_a_Blob
	blobs, otherParams, err := blobstore.ParseUpload(r)
	if err != nil {
		return "", nil, err
	}
	file := blobs[name]
	if len(file) == 0 {
		return "", otherParams, nil
	}
	return string(file[0].BlobKey), otherParams, nil
}

func (a *GaeDatastoreAccessor) processUploadFiles(r *http.Request, names []string) ([]string, map[string][]string, error) {
	blobs, otherParams, err := blobstore.ParseUpload(r)
	if err != nil {
		return nil, nil, err
	}
	blobKeys := []string{}
	for _, name := range names {
		if file := blobs[name]; len(file) > 0 {
			blobKeys = append(blobKeys, string(file[0].BlobKey))
		}
	}
	return blobKeys, otherParams, nil
}

func (a *GaeDatastoreAccessor) nextIdiomID(c appengine.Context) (int, error) {
	q := datastore.NewQuery("Idiom").Order("-Id"). /*.Project("Id")*/ Limit(1)
	it := q.Run(c)
	var maxIdiom Idiom
	_, err := it.Next(&maxIdiom)
	if err == datastore.Done {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	newID := maxIdiom.Id + 1
	return newID, nil
}

func (a *GaeDatastoreAccessor) nextImplID(c appengine.Context) (int, error) {
	// This is not scalable and may yield the same id twice.
	// TODO cleanup this mess.
	// order by implId desc : is it still ok with multi-valued implId ...?
	q := datastore.NewQuery("Idiom").Order("-Implementations.Id"). /*.Project("Implementations.Id")*/ Limit(1)
	it := q.Run(c)
	var maxIdiom Idiom
	_, err := it.Next(&maxIdiom)
	if err == datastore.Done {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if len(maxIdiom.Implementations) == 0 {
		return 0, fmt.Errorf("Existing idiom %d should not have zero impl", maxIdiom.Id)
	}
	maxImplID := -1
	for j := range maxIdiom.Implementations {
		if maxIdiom.Implementations[j].Id > maxImplID {
			maxImplID = maxIdiom.Implementations[j].Id
		}
	}
	newID := maxImplID + 1

	if _, _, err := a.getIdiomByImplID(c, newID); err == nil {
		return 0, fmt.Errorf("Impl %d already exists :(", newID)
	}
	return newID, nil
}

func (a *GaeDatastoreAccessor) languagesHavingImpl(c appengine.Context) []string {
	q := datastore.NewQuery("Idiom").Project("Implementations.LanguageName").Distinct()
	idioms := make([]*Idiom, 0, 40)
	q.GetAll(c, &idioms)
	languages := make([]string, len(idioms))
	for i, idiom := range idioms {
		languages[i] = idiom.Implementations[0].LanguageName
	}
	return languages
}

func (a *GaeDatastoreAccessor) recentIdioms(c appengine.Context, favoriteLangs []string, showOther bool, n int) ([]*Idiom, error) {
	idioms, err := a.idiomsFilterOrder(c, favoriteLangs, n, showOther, "-VersionDate")
	if err != nil {
		return idioms, err
	}
	sortIdiomsByVersionDate(idioms)
	if len(idioms) > n {
		idioms = idioms[0:n]
	}
	return idioms, err
}

func (a *GaeDatastoreAccessor) popularIdioms(c appengine.Context, favoriteLangs []string, showOther bool, n int) ([]*Idiom, error) {
	idioms, err := a.idiomsFilterOrder(c, favoriteLangs, n, showOther, "-Rating")
	if err != nil {
		return idioms, err
	}
	sortIdiomsByRating(idioms)
	if len(idioms) > n {
		idioms = idioms[0:n]
	}
	return idioms, err
}

// Makes one datastore Query for each favorite language with specified sortOrder, then one Query without a language filter.
// Then concatenates the results (eliminating duplicates).
func (a *GaeDatastoreAccessor) idiomsFilterOrder(c appengine.Context, favoriteLangs []string, limitEachLang int, showOther bool, sortOrder string) ([]*Idiom, error) {
	idiomsResult := make([]*Idiom, 0, limitEachLang*len(favoriteLangs))

	langFilters := make([]string, len(favoriteLangs))
	copy(langFilters, favoriteLangs)
	if showOther {
		langFilters = append(langFilters, "") // 1 extra dummy for "no filter"
	}

	idSet := map[int]struct{}{} // To evict duplicates

	for _, lg := range langFilters {
		q := datastore.NewQuery("Idiom")
		if lg != "" {
			q = q.Filter("Implementations.LanguageName =", lg)
		}
		q = q.Order(sortOrder).Order("-Id").Limit(limitEachLang)
		idioms := make([]*Idiom, 0, limitEachLang)
		if _, err := q.GetAll(c, &idioms); err != nil {
			return nil, err
		}
		for _, idiom := range idioms {
			if _, seen := idSet[idiom.Id]; !seen {
				idiomsResult = append(idiomsResult, idiom)
				idSet[idiom.Id] = struct{}{}
			}
		}
	}

	for _, idiom := range idiomsResult {
		seeNonFavorite := true // TODO extract from soft profile!!
		// Inside each Idiom, sort Implementations according to favorites
		implFavoriteLanguagesFirstWithOrder(idiom, favoriteLangs, "", seeNonFavorite)
	}

	return idiomsResult, nil
}

func (a *GaeDatastoreAccessor) randomIdiom(c appengine.Context) (*datastore.Key, *Idiom, error) {
	q := datastore.NewQuery("Idiom")
	//q := q.KeysOnly()
	//keys, err := q.GetAll(c, nil)
	count, err := q.Count(c)
	if err != nil {
		return nil, nil, err
	}
	k := rand.Intn(count)
	q = q.Offset(k).Limit(1)
	idioms := make([]*Idiom, 0, 1)
	keys, err := q.GetAll(c, &idioms)
	if err != nil {
		return nil, nil, err
	}
	return keys[0], idioms[0], err
}

// AppConfigProperty is a (global) application property
type AppConfigProperty struct {
	AppConfigId int
	Name        string
	Value       bool
}

func (a *GaeDatastoreAccessor) getAppConfig(c appengine.Context) (ApplicationConfig, error) {
	q := datastore.NewQuery("AppConfigProperty") // TODO .Filter("AppConfigId =", appConfigId)
	properties := make([]*AppConfigProperty, 0, 100)
	_, err := q.GetAll(c, &properties)
	if err != nil {
		return ApplicationConfig{}, err
	}
	if len(properties) == 0 {
		return ApplicationConfig{}, fmt.Errorf("Found zero AppConfigProperty in the datastore.")
	}

	appConfig := ApplicationConfig{
		Id:      0, // TODO appConfigId
		Toggles: make(Toggles, len(properties)),
	}
	for _, prop := range properties {
		appConfig.Toggles[prop.Name] = prop.Value
	}
	return appConfig, nil
}

func (a *GaeDatastoreAccessor) saveAppConfig(c appengine.Context, appConfig ApplicationConfig) error {
	keys := make([]*datastore.Key, 0, len(appConfig.Toggles))
	properties := make([]*AppConfigProperty, len(appConfig.Toggles))
	i := 0
	for name, value := range appConfig.Toggles {
		key := datastore.NewIncompleteKey(c, "AppConfigProperty", nil)
		keys = append(keys, key)
		prop := AppConfigProperty{
			AppConfigId: 0, // TODO: appConfigId
			Name:        name,
			Value:       value,
		}
		properties[i] = &prop
		i++
	}
	_, err := datastore.PutMulti(c, keys, properties)
	return err
}