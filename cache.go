package tucana

import (
	"context"
	"fmt"
	"github.com/garyburd/redigo/redis"
	jsoniter "github.com/json-iterator/go"
	localCache "github.com/patrickmn/go-cache"
	"golang.org/x/sync/singleflight"
	"time"
)

type CacheUpdateFunc func(ctx context.Context) (interface{}, error)
type Option func(*tCache)
type operation string

const (
	updatingChanName = "cnel:updating:%s" //cnel:updating:{id}，通知该应用id下的缓存更新

	pingTicking = "PING_TICKING"

	commandDel        = "DEL"
	chanMessageFormat = "%s|%s" //key|operation

	defaultExpireIn = 10 * time.Second

	layerLocal  = 1
	layerRemote = 2
)

var empty = []byte("_n")

type fetcher struct {
	fetchFunc fetchFunc
	key       string
	tc        *tCache
}

type fetchFunc func() (cachedContent []byte, isEmpty bool, err error)

type CacheOption struct {
	JsonParser      jsoniter.API
	Layer           int           //缓存层级
	DefaultExpireIn time.Duration //默认过期时间，空数据时使用该值
}

//缓存对象
type tCache struct {
	option     *CacheOption
	m          *manager
	localCache *localCache.Cache

	fetcher fetchFunc
	watchC  chan alteration //key值变动的通知channel
	sf      singleflight.Group
}

func New() *tCache {
	if mgr == nil {
		panic("Init first")
	}

	tc := &tCache{
		option: &CacheOption{
			JsonParser:      jsoniter.ConfigCompatibleWithStandardLibrary,
			Layer:           layerLocal,
			DefaultExpireIn: defaultExpireIn,
		},
		m:          mgr.manager,
		localCache: localCache.New(1*time.Minute, 5*time.Minute),
		fetcher:    nil,
		watchC:     make(chan alteration, 10),
		sf:         singleflight.Group{},
	}

	go tc.watch()

	mgr.registerWatch(tc.watchC) ////注册cache的通知channel

	return tc
}

func (t *tCache) WithOptions(options ...Option) {
	for _, o := range options {
		o(t)
	}
}

func WithDefaultExpireIn(In time.Duration) Option {
	return func(t *tCache) {
		t.option.DefaultExpireIn = In
	}
}

func WithLayer(layer int) Option {
	return func(t *tCache) {
		t.option.Layer = layer
	}
}

func WithOptions(o CacheOption) Option {
	return func(t *tCache) {
		t.option = &o
	}
}

//Storing data into cache
func (t *tCache) store(key string, bdata []byte, layer int) error {
	expireIn := t.option.DefaultExpireIn

	switch layer {
	case layerLocal:
		t.setLocal(key, bdata, expireIn)
		return nil
	case layerRemote:
		//just one shot, ignore if it's failed
		_, err := t.setRemote(key, bdata, expireIn, false)
		return err
	case layerLocal | layerRemote:
		ok, err := t.setRemote(key, bdata, expireIn, false)
		if err != nil {
			return err
		}
		//setting local cache if remote cache was set
		if ok {
			t.setLocal(key, bdata, expireIn)
			return nil
		}
	}

	return fmt.Errorf("layer err layer=%d", layer)
}

//Fetching data from Fetcher
func (t *tCache) pull(fetcher fetchFunc) (interface{}, bool, error) {
	data, isNil, err := fetcher()
	if err != nil {
		return nil, false, err
	}

	return data, isNil, err
}

func (t *tCache) isNil(raw interface{}) bool {
	switch raw.(type) {
	case []byte:
		return string(raw.([]byte)) == string(empty)
	default:
		return false
	}
}
func (t *tCache) nil() []byte {
	return empty
}

// load Fetching data from source and fill it into cache
func (t *tCache) load(key string, fetcher fetchFunc) ([]byte, bool, error) {
	//fetch data from datasource
	//singleflight 防止数据源被压垮
	//从数据源拉取数据
	data, err, _ := t.sf.Do(key, func() (interface{}, error) {
		//在本次读取新数据时，把上一次的旧数据清除，节约内存
		t.sf.Forget(key)
		data, isNil, err := t.pull(fetcher)
		if err != nil {
			return t.nil(), err
		}

		if isNil { //数据源为空
			return t.nil(), nil
		}

		return data, nil
	})
	if err != nil {
		return t.nil(), false, err
	}

	//set a default value nil against the illegal
	//设置默认值，防止缓存穿透
	bdata := t.nil()
	if !t.isNil(data) { //数据源非空
		bdata = data.([]byte)
		//bdata, err = json.Marshal(data)
		//if err != nil {
		//	return nil, false, fmt.Errorf("the returning data from fetcher is not a struct, json err=%s",err)
		//}
	}

	return bdata, false, nil
}

//getting cache cascaded
func (t *tCache) getCascade(key string, layer int, fresh bool) (bdata []byte, ok bool, err error) {
	switch layer {
	case layerLocal: //从本地获取缓存
		bdata, ok = t.getLocal(key)
		if ok {
			return bdata, ok, nil
		}
	case layerRemote: //从远程rds获取缓存
		bdata, ok, err = t.getRemote(key)
		if ok || err == nil {
			return bdata, ok, nil
		}
	case layerRemote | layerLocal: //先从本地获取缓存，再从远程rds获取缓存
		bdata, ok = t.getLocal(key)
		if ok {
			return bdata, ok, nil
		}

		bdata, ok, err = t.getRemote(key)
		if ok || err == nil {

			if fresh {
				//更新本地缓存
				t.setLocal(key, bdata, t.option.DefaultExpireIn)
			}

			return bdata, ok, err
		}
	}

	return t.nil(), false, fmt.Errorf("unsupporting layer for=%d", layer)
}

//设置本地缓存
// setLocal Setting local cache
func (t *tCache) setLocal(key string, obj interface{}, expireIn time.Duration) {
	switch obj.(type) {
	case []byte:
		t.localCache.Set(key, obj.([]byte), expireIn)
		return
	}

	//w := &bytes.Buffer{}
	//dec := gob.NewEncoder(	w)
	//err := dec.Encode(&obj)
	//if err != nil {
	//	fmt.Println(fmt.Sprintf("setLocal key=%s, err=%s", key, err))
	//	return err
	//}
	//t.localCache.Set(key, w.Bytes(), expireIn)

	bdata, _ := t.option.JsonParser.Marshal(obj)
	t.localCache.Set(key, bdata, expireIn)
	return
}

func (t *tCache) getLocal(key string) ([]byte, bool) {
	data, ok := t.localCache.Get(key)
	if ok {
		if t.isNil(data.([]byte)) {
			return nil, false
		}
		return data.([]byte), true
	}
	return nil, false
}

// setting remote cache
func (t *tCache) setRemote(key string, data []byte, expireIn time.Duration, isForce bool) (ok bool, err error) {
	var ret string
	if isForce {
		ret, err = redis.String(t.m.rdsPool.Get().Do("SET", key, data, "PX", expireIn.Nanoseconds()/1e6))
	} else {
		ret, err = redis.String(t.m.rdsPool.Get().Do("SET", key, data, "NX", "PX", expireIn.Nanoseconds()/1e6))
	}

	if err != nil {
		return false, err
	}

	return ret == "OK", nil
}

// getRemote getting the key's value from remote cache
func (t *tCache) getRemote(key string) ([]byte, bool, error) {
	//typ := reflect.TypeOf(obj)
	//if typ == nil || typ.Kind() != reflect.Ptr {
	//	return nil, false, fmt.Errorf("can only parse into pointer")
	//}

	//remote mem, the cache for the second layer
	raw, err := redis.Bytes(t.m.rdsPool.Get().Do("GET", key))
	if err != nil {
		return nil, false, err
	}

	if len(raw) == 0 {
		return nil, false, nil
	}

	// it might be the
	if t.isNil(raw) {
		return nil, true, nil
	}

	//r := bytes.NewBuffer(raw)
	//dec := gob.NewDecoder(r)
	//err = dec.Decode(&obj)
	//if err != nil {
	//	return nil, false, err
	//}

	return raw, err == nil, err
}

func (t *tCache) purgeLocal(key string) {
	t.localCache.Delete(key)
}

func (t *tCache) purgeRemote(key string) {
	_, e := t.m.rdsPool.Get().Do("DEL", key)
	if e != nil {
		fmt.Printf("purgeRemote key=%s, err=%s", key, e)
	}
}

func (t *tCache) watch() {
	for alteration := range t.watchC {
		if alteration.oper == commandDel {
			t.purgeRemote(alteration.key)
			t.purgeLocal(alteration.key)
		}
	}

	fmt.Println("stop watch")
}

func (t *tCache) IsNil(raw interface{}) bool {
	return t.isNil(raw)
}

func (t *tCache) Update(ctx context.Context, tag string, argus ...interface{}) error {
	//notify key to update
	return t.m.NotifyUpdating(fmt.Sprintf(tag, argus...))
}

func (t *tCache) Store(key string, bdata []byte) error {
	return t.store(key, bdata, t.option.Layer)
}
func (t *tCache) StoreLocal(key string, bdata []byte) error {
	return t.store(key, bdata, layerLocal)
}
func (t *tCache) StoreMem(key string, bdata []byte) error {
	return t.store(key, bdata, layerRemote)
}
func (t *tCache) StoreBoth(key string, bdata []byte) error {
	return t.store(key, bdata, layerLocal|layerRemote)
}

func (t *tCache) GetOrFetch(key string, fetcher fetchFunc, expireIn time.Duration) ([]byte, bool, error) {
	//级联获取
	data, ok, err := t.getCascade(key, t.option.Layer, true)
	if err != nil {
		return nil, false, err
	}
	if ok {
		return data, true, nil
	}

	//loading data from src
	data, ok, err = t.load(key, fetcher)
	if err != nil || !ok {
		return nil, false, err
	}

	t.store(key, data, t.option.Layer)

	return data, true, nil
}

func (t *tCache) Get(tag string, argus ...interface{}) *tagCache {
	return &tagCache{
		l:   fromCache,
		key: fmt.Sprintf(tag, argus...),
		t:   t,
	}
}

func (t *tCache) OrFetch(fetcher fetchFunc) *tagCache {
	t.fetcher = fetcher
	return &tagCache{
		l:   fromSrc,
		key: "",
		t:   t,
	}
}

const (
	fromCache = 1
	fromSrc   = 2
)

type tagCache struct {
	t   *tCache
	l   int
	key string
}

func (tc *tagCache) Get(tag string, argus ...interface{}) *tagCache {
	tc.l |= fromCache
	if len(argus) != 0 {
		tc.key = fmt.Sprintf(tag, argus...)
	}

	return tc
}

func (tc *tagCache) OrFetch(fetcher fetchFunc) *tagCache {
	tc.l |= fromSrc
	tc.t.fetcher = fetcher
	return tc
}

func (tc *tagCache) Do() ([]byte, bool, error) {
	if tc.isInvalidKey() {
		return []byte{}, false, fmt.Errorf("key empty")
	}

	switch tc.l {
	case fromCache:
		return tc.get()
	case fromSrc:
		data, ok, err := tc.load()
		if err == nil {
			tc.t.store(tc.key, data, tc.t.option.Layer)
		}
		return data, ok, err
	case fromCache | fromSrc:
		data, ok, err := tc.get()
		if !ok {
			data, ok, err = tc.load()
			if err == nil {
				tc.t.store(tc.key, data, tc.t.option.Layer)
			}
		}

		return data, ok, err
	}

	return []byte{}, false, nil
}

func (tc *tagCache) load() ([]byte, bool, error) {
	//loading data from src
	if tc.t.fetcher == nil {
		return []byte{}, false, nil
	}

	data, ok, err := tc.t.load(tc.key, tc.t.fetcher)
	if err != nil || !ok {
		return nil, false, err
	}

	return data, true, nil
}

func (tc *tagCache) get() ([]byte, bool, error) {
	//级联获取
	data, ok, err := tc.t.getCascade(tc.key, tc.t.option.Layer, true)
	if err != nil {
		return nil, false, err
	}
	if ok {
		return data, true, nil
	}

	return []byte{}, false, nil
}

func (tc *tagCache) isInvalidKey() bool {
	return len(tc.key) == 0
}
