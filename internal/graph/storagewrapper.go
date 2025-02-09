package graph

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/internal/build"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/tuple"
)

var (
	// Ensures that Datastore implements the OpenFGADatastore interface.
	_ storage.OpenFGADatastore = (*CachedDatastore)(nil)

	tuplesCacheTotalCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: build.ProjectName,
		Name:      "tuples_cache_total_count",
		Help:      "The total number of created cached iterator instances.",
	}, []string{"operation"})

	tuplesCacheHitCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: build.ProjectName,
		Name:      "tuples_cache_hit_count",
		Help:      "The total number of cache hits from cached iterator instances.",
	}, []string{"operation"})

	tuplesCacheDiscardCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: build.ProjectName,
		Name:      "tuples_cache_discard_count",
		Help:      "The total number of discards from cached iterator instances.",
	}, []string{"operation"})

	tuplesCacheSizeHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:                       build.ProjectName,
		Name:                            "tuples_cache_size",
		Help:                            "The number of tuples cached.",
		Buckets:                         []float64{0, 1, 10, 100, 1000, 5000, 10000},
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	}, []string{"operation"})
)

// iterFunc is a function closure that returns an iterator
// from the underlying datastore.
type iterFunc func(ctx context.Context) (storage.TupleIterator, error)

type CachedDatastore struct {
	storage.OpenFGADatastore

	cache         storage.InMemoryCache[any]
	maxResultSize int
	ttl           time.Duration

	// sf is used to prevent draining the same iterator
	// across multiple requests.
	sf *singleflight.Group
}

// NewCachedDatastore returns a wrapper over a datastore that caches iterators in memory.
func NewCachedDatastore(
	inner storage.OpenFGADatastore,
	cache storage.InMemoryCache[any],
	maxSize int,
	ttl time.Duration,
) *CachedDatastore {
	return &CachedDatastore{
		OpenFGADatastore: inner,
		cache:            cache,
		maxResultSize:    maxSize,
		ttl:              ttl,
		sf:               &singleflight.Group{},
	}
}

func (c *CachedDatastore) ReadStartingWithUser(
	ctx context.Context,
	store string,
	filter storage.ReadStartingWithUserFilter,
	options storage.ReadStartingWithUserOptions,
) (storage.TupleIterator, error) {
	ctx, span := tracer.Start(
		ctx,
		"cache.ReadStartingWithUser",
		trace.WithAttributes(attribute.Bool("cached", false)),
	)
	defer span.End()

	iter := func(ctx context.Context) (storage.TupleIterator, error) {
		return c.OpenFGADatastore.ReadStartingWithUser(ctx, store, filter, options)
	}

	if options.Consistency.Preference == openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY {
		return iter(ctx)
	}

	var b strings.Builder
	b.WriteString(
		storage.GetReadStartingWithUserCacheKeyPrefix(store, filter.ObjectType, filter.Relation),
	)

	// NOTE: while `storagewrapper` is only used in Check there is no need to limit the length of this
	// since at most it will have 2 entries (user and wildcard if possible)
	subjects := make([]string, 0, len(filter.UserFilter))
	for _, objectRel := range filter.UserFilter {
		subject := tuple.ToObjectRelationString(objectRel.GetObject(), objectRel.GetRelation())
		subjects = append(subjects, subject)
		b.WriteString(fmt.Sprintf("/%s", subject))
	}

	if filter.ObjectIDs != nil {
		hasher := xxhash.New()
		for _, oid := range filter.ObjectIDs.Values() {
			if _, err := hasher.WriteString(oid); err != nil {
				return nil, err
			}
		}

		b.WriteString(fmt.Sprintf("/%s", strconv.FormatUint(hasher.Sum64(), 10)))
	}

	return c.newCachedIteratorByUserObjectType(ctx, "ReadStartingWithUser", store, iter, b.String(), subjects, filter.ObjectType)
}

// ReadUsersetTuples see [storage.RelationshipTupleReader].ReadUsersetTuples.
func (c *CachedDatastore) ReadUsersetTuples(
	ctx context.Context,
	store string,
	filter storage.ReadUsersetTuplesFilter,
	options storage.ReadUsersetTuplesOptions,
) (storage.TupleIterator, error) {
	ctx, span := tracer.Start(
		ctx,
		"cache.ReadUsersetTuples",
		trace.WithAttributes(attribute.Bool("cached", false)),
	)
	defer span.End()

	iter := func(ctx context.Context) (storage.TupleIterator, error) {
		return c.OpenFGADatastore.ReadUsersetTuples(ctx, store, filter, options)
	}

	if options.Consistency.Preference == openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY {
		return iter(ctx)
	}

	var b strings.Builder
	b.WriteString(
		storage.GetReadUsersetTuplesCacheKeyPrefix(store, filter.Object, filter.Relation),
	)

	var rb strings.Builder
	var wb strings.Builder

	for _, userset := range filter.AllowedUserTypeRestrictions {
		if _, ok := userset.GetRelationOrWildcard().(*openfgav1.RelationReference_Relation); ok {
			rb.WriteString(fmt.Sprintf("/%s#%s", userset.GetType(), userset.GetRelation()))
		}
		if _, ok := userset.GetRelationOrWildcard().(*openfgav1.RelationReference_Wildcard); ok {
			wb.WriteString(fmt.Sprintf("/%s:*", userset.GetType()))
		}
	}

	// wildcard should have precedence
	if wb.Len() > 0 {
		b.WriteString(wb.String())
	}

	if rb.Len() > 0 {
		b.WriteString(rb.String())
	}

	return c.newCachedIteratorByObjectRelation(ctx, "ReadUsersetTuples", store, iter, b.String(), filter.Object, filter.Relation)
}

// Read see [storage.RelationshipTupleReader].Read.
func (c *CachedDatastore) Read(
	ctx context.Context,
	store string,
	tupleKey *openfgav1.TupleKey,
	options storage.ReadOptions,
) (storage.TupleIterator, error) {
	ctx, span := tracer.Start(
		ctx,
		"cache.Read",
		trace.WithAttributes(attribute.Bool("cached", false)),
	)
	defer span.End()

	iter := func(ctx context.Context) (storage.TupleIterator, error) {
		return c.OpenFGADatastore.Read(ctx, store, tupleKey, options)
	}

	// this instance of Read is only called from TTU resolution path which always includes Object/Relation
	if tupleKey.GetRelation() == "" || !tuple.IsValidObject(tupleKey.GetObject()) {
		return iter(ctx)
	}

	if options.Consistency.Preference == openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY {
		return iter(ctx)
	}

	var b strings.Builder
	b.WriteString(
		storage.GetReadCacheKey(store, tuple.TupleKeyToString(tupleKey)),
	)
	return c.newCachedIteratorByObjectRelation(ctx, "Read", store, iter, b.String(), tupleKey.GetObject(), tupleKey.GetRelation())
}

func (c *CachedDatastore) findInCache(store, key string, invalidEntityKeys []string) (*storage.TupleIteratorCacheEntry, bool) {
	var tupleEntry *storage.TupleIteratorCacheEntry
	if res := c.cache.Get(key); res != nil {
		tupleEntry = res.(*storage.TupleIteratorCacheEntry)
	} else {
		return nil, false
	}
	invalidCacheKey := storage.GetInvalidIteratorCacheKey(store)
	if res := c.cache.Get(invalidCacheKey); res != nil {
		invalidEntry := res.(*storage.InvalidEntityCacheEntry)
		if tupleEntry.LastModified.Before(invalidEntry.LastModified) {
			return nil, false
		}
	}
	for _, invalidEntityKey := range invalidEntityKeys {
		if res := c.cache.Get(invalidEntityKey); res != nil {
			invalidEntry := res.(*storage.InvalidEntityCacheEntry)
			if tupleEntry.LastModified.Before(invalidEntry.LastModified) {
				return nil, false
			}
		}
	}
	return tupleEntry, true
}

func (c *CachedDatastore) newCachedIteratorByObjectRelation(
	ctx context.Context,
	operation string,
	store string,
	dsIterFunc iterFunc,
	cacheKey string,
	object string,
	relation string,
) (storage.TupleIterator, error) {
	objectType, objectID := tuple.SplitObject(object)
	invalidEntityKeys := storage.GetInvalidIteratorByObjectRelationCacheKeys(store, object, relation)
	return c.newCachedIterator(ctx, operation, store, dsIterFunc, cacheKey, invalidEntityKeys, objectType, objectID, relation, "")
}

func (c *CachedDatastore) newCachedIteratorByUserObjectType(
	ctx context.Context,
	operation string,
	store string,
	dsIterFunc iterFunc,
	cacheKey string,
	users []string,
	objectType string,
) (storage.TupleIterator, error) {
	// if all users in filter are of the same type, we can store in cache without the value
	var userType string
	for _, user := range users {
		userObjectType, _ := tuple.SplitObject(user)
		if userType == "" {
			userType = userObjectType
		} else if userType != userObjectType {
			userType = ""
			break
		}
	}

	invalidEntityKeys := storage.GetInvalidIteratorByUserObjectTypeCacheKeys(store, users, objectType)
	return c.newCachedIterator(ctx, operation, store, dsIterFunc, cacheKey, invalidEntityKeys, objectType, "", "", userType)
}

// newCachedIterator either returns a cached static iterator for a cache hit, or
// returns a new iterator that attempts to cache the results.
func (c *CachedDatastore) newCachedIterator(
	ctx context.Context,
	operation string,
	store string,
	dsIterFunc iterFunc,
	cacheKey string,
	invalidEntityKeys []string,
	objectType string,
	objectID string,
	relation string,
	userType string,
) (storage.TupleIterator, error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("cache_key", cacheKey))
	tuplesCacheTotalCounter.WithLabelValues(operation).Inc()

	if cacheEntry, ok := c.findInCache(store, cacheKey, invalidEntityKeys); ok {
		tuplesCacheHitCounter.WithLabelValues(operation).Inc()
		span.SetAttributes(attribute.Bool("cached", true))

		staticIter := storage.NewStaticIterator[*storage.TupleRecord](cacheEntry.Tuples)

		return &cachedTupleIterator{
			objectID:   objectID,
			objectType: objectType,
			relation:   relation,
			userType:   userType,
			iter:       staticIter,
		}, nil
	}

	iter, err := dsIterFunc(ctx)
	if err != nil {
		return nil, err
	}

	return &cachedIterator{
		iter:      iter,
		operation: operation,
		// set an initial fraction capacity to balance constant reallocation and memory usage
		tuples:            make([]*openfgav1.Tuple, 0, c.maxResultSize/2),
		cacheKey:          cacheKey,
		invalidEntityKeys: invalidEntityKeys,
		cache:             c.cache,
		maxResultSize:     c.maxResultSize,
		ttl:               c.ttl,
		sf:                c.sf,
		objectType:        objectType,
		objectID:          objectID,
		relation:          relation,
		userType:          userType,
	}, nil
}

// Close closes the datastore and cleans up any residual resources.
func (c *CachedDatastore) Close() {
	c.OpenFGADatastore.Close()
}

type cachedIterator struct {
	iter              storage.TupleIterator
	operation         string
	cacheKey          string
	invalidEntityKeys []string
	cache             storage.InMemoryCache[any]
	ttl               time.Duration

	objectID   string
	objectType string
	relation   string
	userType   string

	// tuples is used to buffer tuples as they are read from `iter`.
	tuples []*openfgav1.Tuple

	// records is used to buffer tuples that might end up in cache.
	records []*storage.TupleRecord

	// maxResultSize is the maximum number of tuples to cache. If the number
	// of tuples found exceeds this value, it will not be cached.
	maxResultSize int

	// sf is used to prevent draining the same iterator
	// across multiple requests.
	sf *singleflight.Group

	// closeOnce is used to synchronize `.Close()` and ensure and stop
	// producing tuples it's only done once.
	closing atomic.Bool

	// mu is used to synchronize access to the iterator.
	mu sync.Mutex

	// wg is used purely for testing and is an internal detail.
	wg sync.WaitGroup
}

// Next will return the next available tuple from the underlying iterator and
// will attempt to add to buffer if not yet full. To set buffered tuples in cache,
// you must call .Stop().
func (c *cachedIterator) Next(ctx context.Context) (*openfgav1.Tuple, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing.Load() {
		return nil, storage.ErrIteratorDone
	}

	t, err := c.iter.Next(ctx)
	if err != nil {
		if !storage.IterIsDoneOrCancelled(err) {
			c.tuples = nil // don't store results that are incomplete
		}
		return nil, err
	}

	if c.tuples != nil {
		c.tuples = append(c.tuples, t)
		if len(c.tuples) >= c.maxResultSize {
			c.tuples = nil // don't store results that are incomplete
		}
	}

	return t, nil
}

// Stop terminates iteration over the underlying iterator.
//   - If there are incomplete results, they will not be cached.
//   - If the iterator is already fully consumed, it will be cached in the foreground.
//   - If the iterator is not fully consumed, it will be drained in the background,
//     and attempt will be made to cache its results.
func (c *cachedIterator) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	swapped := c.closing.CompareAndSwap(false, true)
	if !swapped {
		return
	}

	if c.tuples == nil {
		c.iter.Stop()
		return
	}

	// if cache is already set, we don't need to drain the iterator
	if cachedResp := c.cache.Get(c.cacheKey); cachedResp != nil {
		c.iter.Stop()
		c.tuples = nil
		return
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.iter.Stop()

		c.records = make([]*storage.TupleRecord, 0, len(c.tuples))

		for _, t := range c.tuples {
			c.addToBuffer(t)
		}

		// prevent goroutine if iterator was already consumed
		ctx := context.Background()
		if _, err := c.iter.Head(ctx); errors.Is(err, storage.ErrIteratorDone) {
			c.flush()
			return
		}

		// prevent draining on the same iterator across multiple requests
		_, _, _ = c.sf.Do(c.cacheKey, func() (interface{}, error) {
			for {
				// attempt to drain the iterator to have it ready for subsequent calls
				t, err := c.iter.Next(ctx)
				if err != nil {
					if errors.Is(err, storage.ErrIteratorDone) {
						c.flush()
					}
					break
				}
				// if the size is exceeded we don't add anymore and exit
				if !c.addToBuffer(t) {
					break
				}
			}
			return nil, nil
		})
	}()
}

// Head see [storage.Iterator].Head.
func (c *cachedIterator) Head(ctx context.Context) (*openfgav1.Tuple, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing.Load() {
		return nil, storage.ErrIteratorDone
	}

	return c.iter.Head(ctx)
}

// addToBuffer converts a proto tuple into a simpler storage.TupleRecord, removes
// any already known fields and adds it to the buffer if not yet full.
func (c *cachedIterator) addToBuffer(t *openfgav1.Tuple) bool {
	if c.tuples == nil {
		return false
	}

	tk := t.GetKey()
	object := tk.GetObject()
	objectType, objectID := tuple.SplitObject(object)
	userObjectType, userObjectID, userRelation := tuple.ToUserParts(tk.GetUser())

	record := &storage.TupleRecord{
		ObjectID:       objectID,
		ObjectType:     objectType,
		Relation:       tk.GetRelation(),
		UserObjectType: userObjectType,
		UserObjectID:   userObjectID,
		UserRelation:   userRelation,
	}

	if timestamp := t.GetTimestamp(); timestamp != nil {
		record.InsertedAt = timestamp.AsTime()
	}

	if condition := tk.GetCondition(); condition != nil {
		record.ConditionName = condition.GetName()
		record.ConditionContext = condition.GetContext()
	}

	// Remove any fields that are duplicated and known by iterator
	if c.objectID != "" && c.objectID == record.ObjectID {
		record.ObjectID = ""
	}
	if c.objectType != "" && c.objectType == record.ObjectType {
		record.ObjectType = ""
	}
	if c.relation != "" && c.relation == record.Relation {
		record.Relation = ""
	}
	if c.userType != "" && c.userType == record.UserObjectType {
		record.UserObjectType = ""
	}

	c.records = append(c.records, record)

	if len(c.records) >= c.maxResultSize {
		tuplesCacheDiscardCounter.WithLabelValues(c.operation).Inc()
		c.tuples = nil
		c.records = nil
	}

	return true
}

// flush will store copy of buffered tuples into cache.
func (c *cachedIterator) flush() {
	if c.tuples == nil {
		return
	}

	// Copy tuples buffer into new destination before storing into cache
	// otherwise, the cache will be storing pointers. This should also help
	// with garbage collection.
	records := c.records
	c.tuples = nil
	c.records = nil

	c.cache.Set(c.cacheKey, &storage.TupleIteratorCacheEntry{Tuples: records, LastModified: time.Now()}, c.ttl)
	for _, k := range c.invalidEntityKeys {
		c.cache.Delete(k)
	}
	tuplesCacheSizeHistogram.WithLabelValues(c.operation).Observe(float64(len(records)))
}
