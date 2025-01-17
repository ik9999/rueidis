package om

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/rueian/rueidis"
)

// NewJSONRepository creates an JSONRepository.
// The prefix parameter is used as redis key prefix. The entity stored by the repository will be named in the form of `{prefix}:{id}`
// The schema parameter should be a struct with fields tagged with `redis:",key"` and `redis:",ver"`
func NewJSONRepository(prefix string, schema interface{}, client rueidis.Client) Repository {
	repo := &JSONRepository{
		prefix: prefix,
		idx:    "jsonidx:" + prefix,
		typ:    reflect.TypeOf(schema),
		client: client,
	}
	repo.schema = newSchema(repo.typ)
	return repo
}

var _ Repository = (*JSONRepository)(nil)

// JSONRepository is an OM repository backed by RedisJSON.
type JSONRepository struct {
	schema schema
	typ    reflect.Type
	client rueidis.Client
	prefix string
	idx    string
}

// NewEntity returns an empty entity which type is `*{schema}` and will have the `redis:",key"` field be set with ULID automatically.
func (r *JSONRepository) NewEntity() (entity interface{}) {
	v := reflect.New(r.typ)
	v.Elem().Field(r.schema.key.idx).Set(reflect.ValueOf(id()))
	return v.Interface()
}

// Fetch an entity whose name is `{prefix}:{id}` and returns as `*{schema}`
func (r *JSONRepository) Fetch(ctx context.Context, id string) (interface{}, error) {
	record, err := r.client.Do(ctx, r.client.B().JsonGet().Key(key(r.prefix, id)).Build()).ToString()
	if err != nil {
		return nil, err
	}
	iface, _, err := r.decode(record)
	return iface, err
}

// FetchCache is like Fetch, but it uses client side caching mechanism.
func (r *JSONRepository) FetchCache(ctx context.Context, id string, ttl time.Duration) (v interface{}, err error) {
	record, err := r.client.DoCache(ctx, r.client.B().JsonGet().Key(key(r.prefix, id)).Cache(), ttl).ToString()
	if err != nil {
		return nil, err
	}
	iface, _, err := r.decode(record)
	return iface, err
}

func (r *JSONRepository) decode(record string) (interface{}, reflect.Value, error) {
	val := reflect.New(r.typ)
	iface := val.Interface()
	if err := json.NewDecoder(strings.NewReader(record)).Decode(iface); err != nil {
		return nil, reflect.Value{}, err
	}
	return iface, val, nil
}

// Save the entity under the redis key of `{prefix}:{id}`.
// It also uses the `redis:",ver"` field and lua script to perform optimistic locking and prevent lost update.
func (r *JSONRepository) Save(ctx context.Context, entity interface{}) (err error) {
	val, ok := ptrValueOf(entity, r.typ)
	if !ok {
		panic(fmt.Sprintf("input entity should be a pointer to %v", r.typ))
	}

	keyField := val.Field(r.schema.key.idx)
	verField := val.Field(r.schema.ver.idx)

	sb := strings.Builder{}
	if err = json.NewEncoder(&sb).Encode(entity); err != nil {
		return err
	}

	str, err := jsonSaveScript.Exec(ctx, r.client, []string{key(r.prefix, keyField.String())}, []string{
		r.schema.ver.name, strconv.FormatInt(verField.Int(), 10), sb.String(),
	}).ToString()
	if rueidis.IsRedisNil(err) {
		return ErrVersionMismatch
	}
	if err != nil {
		return err
	}
	ver, _ := strconv.ParseInt(str, 10, 64)
	verField.SetInt(ver)
	return nil
}

// Remove the entity under the redis key of `{prefix}:{id}`.
func (r *JSONRepository) Remove(ctx context.Context, id string) error {
	return r.client.Do(ctx, r.client.B().Del().Key(key(r.prefix, id)).Build()).Error()
}

// CreateIndex uses FT.CREATE from the RediSearch module to create inverted index under the name `jsonidx:{prefix}`
// You can use the cmdFn parameter to mutate the index construction command,
// and note that the field name should be specified with JSON path syntax, otherwise the index may not work as expected.
func (r *JSONRepository) CreateIndex(ctx context.Context, cmdFn func(schema FtCreateSchema) Completed) error {
	return r.client.Do(ctx, cmdFn(r.client.B().FtCreate().Index(r.idx).OnJson().Prefix(1).Prefix(r.prefix+":").Schema())).Error()
}

// DropIndex uses FT.DROPINDEX from the RediSearch module to drop index whose name is `jsonidx:{prefix}`
func (r *JSONRepository) DropIndex(ctx context.Context) error {
	return r.client.Do(ctx, r.client.B().FtDropindex().Index(r.idx).Build()).Error()
}

// Search uses FT.SEARCH from the RediSearch module to search the index whose name is `jsonidx:{prefix}`
// It returns three values:
// 1. total count of match results inside the redis, and note that it might be larger than returned search result.
// 2. search result, which type is []*{schema}, and note that its length might smaller than the first return value.
// 3. error if any
// You can use the cmdFn parameter to mutate the search command.
func (r *JSONRepository) Search(ctx context.Context, cmdFn func(search FtSearchIndex) Completed) (int64, interface{}, error) {
	resp, err := r.client.Do(ctx, cmdFn(r.client.B().FtSearch().Index(r.idx))).ToArray()
	if err != nil {
		return 0, nil, err
	}

	n, _ := resp[0].ToInt64()
	s := reflect.MakeSlice(reflect.SliceOf(reflect.PtrTo(r.typ)), 0, len(resp[1:])/2)
	for i := 2; i < len(resp); i += 2 {
		if kv, _ := resp[i].ToArray(); len(kv) == 2 {
			if k, _ := kv[0].ToString(); k == "$" {
				record, _ := kv[1].ToString()
				_, v, err := r.decode(record)
				if err != nil {
					return 0, nil, err
				}
				s = reflect.Append(s, v)
			}
		}
	}
	return n, s.Interface(), nil
}

var jsonSaveScript = rueidis.NewLuaScript(`
local v = redis.call('JSON.GET',KEYS[1],ARGV[1])
if (not v or v == ARGV[2])
then
  redis.call('JSON.SET',KEYS[1],'$',ARGV[3])
  return redis.call('JSON.NUMINCRBY',KEYS[1],ARGV[1],1)
end
return nil
`)
