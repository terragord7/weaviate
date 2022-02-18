package inverted

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/semi-technologies/weaviate/adapters/repos/db/helpers"
	"github.com/semi-technologies/weaviate/adapters/repos/db/lsmkv"
	"github.com/semi-technologies/weaviate/adapters/repos/db/propertyspecific"
	"github.com/semi-technologies/weaviate/entities/additional"
	"github.com/semi-technologies/weaviate/entities/filters"
	"github.com/semi-technologies/weaviate/entities/schema"
	"github.com/semi-technologies/weaviate/entities/storobj"
	"github.com/semi-technologies/weaviate/usecases/traverser"
)

type BM25Searcher struct {
	store         *lsmkv.Store
	schema        schema.Schema
	rowCache      cacher
	classSearcher ClassSearcher // to allow recursive searches on ref-props
	propIndices   propertyspecific.Indices
	deletedDocIDs DeletedDocIDChecker
}

func NewBM25Searcher(store *lsmkv.Store, schema schema.Schema,
	rowCache cacher, propIndices propertyspecific.Indices,
	classSearcher ClassSearcher, deletedDocIDs DeletedDocIDChecker) *BM25Searcher {
	return &BM25Searcher{
		store:         store,
		schema:        schema,
		rowCache:      rowCache,
		propIndices:   propIndices,
		classSearcher: classSearcher,
		deletedDocIDs: deletedDocIDs,
	}
}

// Object returns a list of full objects
func (b *BM25Searcher) Object(ctx context.Context, limit int,
	keywordRanking *traverser.KeywordRankingParams,
	filter *filters.LocalFilter, additional additional.Properties,
	className schema.ClassName) ([]*storobj.Object, error) {
	defer func() {
		err := recover()
		if err != nil {
			fmt.Println(err)
			debug.PrintStack()
		}
	}()

	// TODO: more complex pre-processing with proper split function
	terms := strings.Split(keywordRanking.Query, " ")

	idLists := make([]docPointersWithScore, len(terms))

	for i, term := range terms {
		ids, err := b.retrieveScoreAndSortForSingleTerm(ctx,
			keywordRanking.Properties[0], term)
		if err != nil {
			return nil, err
		}

		idLists[i] = ids
	}

	ids := newScoreMerger(idLists).do()

	sort.Slice(ids.docIDs, func(a, b int) bool {
		return ids.docIDs[a].score > ids.docIDs[b].score
	})

	if len(ids.docIDs) > limit {
		ids.docIDs = ids.docIDs[:limit]
	}

	res, err := b.objectsByDocID(ids.IDs(), additional)
	if err != nil {
		return nil, errors.Wrap(err, "resolve doc ids to objects")
	}

	return res, nil
}

func (b *BM25Searcher) retrieveScoreAndSortForSingleTerm(ctx context.Context,
	property, term string) (docPointersWithScore, error) {
	ids, err := b.getIdsWithFrequenciesForTerm(ctx, property, term)
	if err != nil {
		return docPointersWithScore{}, errors.Wrap(err,
			"read doc ids and their frequencies from inverted index")
	}

	b.score(ids)

	before := time.Now()
	// TODO: this runtime sorting is only because the storage is not implemented
	// in an always sorted manner. Once we have that implemented, we can skip
	// this expensive runtime-sort
	sort.Slice(ids.docIDs, func(a, b int) bool {
		return ids.docIDs[a].id < ids.docIDs[b].id
	})

	// TODO: structured logging
	fmt.Printf("TEMP DEBUG: sorting by doc ids took %s\n", time.Since(before))

	return ids, nil
}

func (bm *BM25Searcher) score(ids docPointersWithScore) {
	averageDocLen := float64(1) // TODO: use real value
	docLen := float64(1)        // TODO: use real value
	k1 := 1.2                   // TODO: make configurable
	b := 0.75                   // TODO: make configurable
	N := float64(bm.store.Bucket(helpers.ObjectsBucketLSM).Count())
	n := float64(len(ids.docIDs))
	idf := math.Log(float64(1) + (N-n+0.5)/(n+0.5))
	for i, id := range ids.docIDs {
		tf := id.frequency / (id.frequency + k1*(1-b+b*docLen/averageDocLen))
		ids.docIDs[i].score = tf * idf
	}
}

func (b *BM25Searcher) getIdsWithFrequenciesForTerm(ctx context.Context,
	prop, term string) (docPointersWithScore, error) {
	bucketName := helpers.BucketFromPropNameLSM(prop)
	bucket := b.store.Bucket(bucketName)

	return b.docPointersInvertedFrequency(prop, bucket, 0, &propValuePair{
		operator: filters.OperatorEqual,
		value:    []byte(term),
		prop:     prop,
	}, true)
}

func (b *BM25Searcher) docPointersInvertedFrequency(prop string, bucket *lsmkv.Bucket,
	limit int, pv *propValuePair, tolerateDuplicates bool) (docPointersWithScore, error) {
	rr := NewRowReaderFrequency(bucket, pv.value, pv.operator, false)

	var pointers docPointersWithScore
	var hashes [][]byte

	if err := rr.Read(context.TODO(), func(k []byte, pairs []lsmkv.MapPair) (bool, error) {
		currentDocIDs := make([]docPointerWithScore, len(pairs))
		// beforePairs := time.Now()
		for i, pair := range pairs {
			currentDocIDs[i].id = binary.LittleEndian.Uint64(pair.Key)
			freqBits := binary.LittleEndian.Uint64(pair.Value)
			currentDocIDs[i].frequency = math.Float64frombits(freqBits)
		}
		// fmt.Printf("loop through pairs took %s\n", time.Since(beforePairs))

		pointers.count += uint64(len(pairs))
		if len(pointers.docIDs) > 0 {
			pointers.docIDs = append(pointers.docIDs, currentDocIDs...)
		} else {
			pointers.docIDs = currentDocIDs
		}

		hashBucket := b.store.Bucket(helpers.HashBucketFromPropNameLSM(pv.prop))
		if b == nil {
			return false, errors.Errorf("no hash bucket for prop '%s' found", pv.prop)
		}

		// use retrieved k instead of pv.value - they are typically the same, but
		// not on a like operator with wildcard where we only had a partial match
		currHash, err := hashBucket.Get(k)
		if err != nil {
			return false, errors.Wrap(err, "get hash")
		}

		hashes = append(hashes, currHash)
		if limit > 0 && pointers.count >= uint64(limit) {
			return false, nil
		}

		return true, nil
	}); err != nil {
		return pointers, errors.Wrap(err, "read row")
	}

	pointers.checksum = combineChecksums(hashes, pv.operator)

	// TODO
	// if !tolerateDuplicates {
	// 	pointers.removeDuplicates()
	// }
	return pointers, nil
}

func (bm *BM25Searcher) objectsByDocID(ids []uint64,
	additional additional.Properties) ([]*storobj.Object, error) {
	out := make([]*storobj.Object, len(ids))

	bucket := bm.store.Bucket(helpers.ObjectsBucketLSM)
	if bucket == nil {
		return nil, errors.Errorf("objects bucket not found")
	}

	i := 0

	for _, id := range ids {
		keyBuf := bytes.NewBuffer(nil)
		binary.Write(keyBuf, binary.LittleEndian, &id)
		docIDBytes := keyBuf.Bytes()
		res, err := bucket.GetBySecondary(0, docIDBytes)
		if err != nil {
			return nil, err
		}

		if res == nil {
			continue
		}

		unmarshalled, err := storobj.FromBinaryOptional(res, additional)
		if err != nil {
			return nil, errors.Wrapf(err, "unmarshal data object at position %d", i)
		}

		out[i] = unmarshalled
		i++
	}

	return out[:i], nil
}