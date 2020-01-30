// Package cache is a caching selector. It uses the registry watcher.
package selector

import (
	"fmt"
	"sync"
	"time"

	"github.com/micro/go-micro/client/selector"
	"github.com/micro/go-micro/registry"
	"github.com/micro/go-micro/util/log"
)

type selectorCache struct {
	so  selector.Options
	ttl time.Duration

	// registry cache
	sync.Mutex

	// cache 中存储了 service name 对于的一组 *registry.Service 结构体，这些结构体之间主要通过 version 来区别

	cache map[string][]*registry.Service
	ttls  map[string]time.Time

	watched map[string]bool

	// used to close or reload watcher
	reload chan bool
	exit   chan bool
}

var (
	DefaultTTL = time.Minute
)

func (c *selectorCache) quit() bool {
	select {
	case <-c.exit:
		return true
	default:
		return false
	}
}

// cp copies a service slice.
//
// Because we're caching handing back pointers would create a race condition,
// so we do this instead its fast enough
//
//
// cp 函数实现 []*registry.Service 数组的深拷贝，避免指针的争用，以单个 *registry.Service 对象来说：
//（1）拷贝 s * registry.Service 结构体
//（2）拷贝 s.Nodes 结构体数组
//（3）拷贝 s.Endpoints 结构体数组
func (c *selectorCache) cp(current []*registry.Service) []*registry.Service {


	var services []*registry.Service

	for _, service := range current {

		// copy service
		s := new(registry.Service)
		*s = *service

		// copy nodes
		var nodes []*registry.Node
		for _, node := range service.Nodes {
			n := new(registry.Node)
			*n = *node
			nodes = append(nodes, n)
		}
		s.Nodes = nodes

		// copy endpoints
		var eps []*registry.Endpoint
		for _, ep := range service.Endpoints {
			e := new(registry.Endpoint)
			*e = *ep
			eps = append(eps, e)
		}
		s.Endpoints = eps

		// append service
		services = append(services, s)
	}

	return services
}




// 获取 service 对应的服务信息，主要是 EndPoints 信息和 nodes 信息。
//
// 1. 检查 service 是否为首次获取，若是则置 c.watched[service] 为 true，同时启动 go c.run(service) 协程进行后台刷新。
// 2. 检查 service 是否已被缓存，缓存结果存储在 c.cache[service] 中，若未被缓存，则直接回源拉取。
// 3. 检查 service 缓存是否过期，缓存 ttl 存储在 c.ttls[service] 中，若未过期，则直接返回缓存内容，否则回源拉取。
// 4. 如果缓存已过期，但是回源失败，当错误码为 "ErrNotFound" 时直接报错，其它错误时，直接返回已过期的内容，作为降级。
func (c *selectorCache) get(service string) ([]*registry.Service, error) {
	c.Lock()
	defer c.Unlock()


	// watch service if not watched
	if _, ok := c.watched[service]; !ok {
		go c.run(service)
		c.watched[service] = true
	}

	// get does the actual request for a service, it also caches it
	get := func(service string) ([]*registry.Service, error) {
		// ask the registry
		services, err := c.so.Registry.GetService(service)
		if err != nil {
			return nil, err
		}

		// cache results
		c.set(service, c.cp(services))

		return services, nil
	}

	// check the cache first
	services, ok := c.cache[service]

	// cache miss or no services
	if !ok || len(services) == 0 {
		return get(service)
	}

	// cache hit, then check if cache is expired
	ttl, kk := c.ttls[service]

	// not expired, so return it directly
	if kk && time.Since(ttl) < 0 {
		return c.cp(services), nil
	}

	// expired, so call get
	services, err := get(service)

	// no error then return error
	if err == nil {
		return services, nil
	}


	// not found error then return
	if err == registry.ErrNotFound {
		return nil, selector.ErrNotFound
	}

	// other error ...

	// return expired cache as last resort
	return c.cp(services), nil
}


// 同时设置 cache 和 ttl
func (c *selectorCache) set(service string, services []*registry.Service) {
	c.cache[service] = services
	c.ttls[service] = time.Now().Add(c.ttl)
}

// 同时删除 cache 和 ttl
func (c *selectorCache) del(service string) {
	delete(c.cache, service)
	delete(c.ttls, service)
}

//
func (c *selectorCache) update(res *registry.Result) {

	if res == nil || res.Service == nil {
		return
	}

	c.Lock()
	defer c.Unlock()


	// 从缓存中获取 res.service.name 的信息，如果缓存内容不存在，则无需更新
	services, ok := c.cache[res.Service.Name]
	if !ok {
		// we're not going to cache anything unless there was already a lookup
		return
	}

	// 如果 res.service.nodes 为空，且事件类型是 "delete"，则删除 cache 和 ttl
	if len(res.Service.Nodes) == 0 {
		switch res.Action {
		case "delete":
			c.del(res.Service.Name)
		}
		return
	}


	// existing service found

	// 根据 res.Service.Version 确定缓存中的具体某个 *registry.Service 对象
	var service *registry.Service
	var index int
	for i, s := range services {
		if s.Version == res.Service.Version {
			service = s
			index = i
		}
	}

	// 根据 res.Action 决定如何处理当前 action
	switch res.Action {
	case "create", "update":

		//
		if service == nil {
			c.set(res.Service.Name, append(services, res.Service))
			return
		}

		// append old nodes to new service
		for _, cur := range service.Nodes {
			var seen bool
			for _, node := range res.Service.Nodes {
				if cur.Id == node.Id {
					seen = true
					break
				}
			}
			if !seen {
				res.Service.Nodes = append(res.Service.Nodes, cur)
			}
		}

		services[index] = res.Service
		c.set(res.Service.Name, services)


	case "delete":
		if service == nil {
			return
		}

		var nodes []*registry.Node

		// filter cur nodes to remove the dead one
		for _, cur := range service.Nodes {
			var seen bool
			for _, del := range res.Service.Nodes {
				if del.Id == cur.Id {
					seen = true
					break
				}
			}
			if !seen {
				nodes = append(nodes, cur)
			}
		}

		// still got nodes, save and return
		if len(nodes) > 0 {
			service.Nodes = nodes
			services[index] = service
			c.set(service.Name, services)
			return
		}

		// zero nodes left

		// only have one thing to delete
		// nuke the thing
		if len(services) == 1 {
			c.del(service.Name)
			return
		}

		// still have more than 1 service
		// check the version and keep what we know
		var srvs []*registry.Service
		for _, s := range services {
			if s.Version != service.Version {
				srvs = append(srvs, s)
			}
		}

		// save
		c.set(service.Name, srvs)
	}
}

// run starts the cache watcher loop
// it creates a new watcher if there's a problem
// reloads the watcher if Init is called
// and returns when Close is called
func (c *selectorCache) run(name string) {


	for {

		fmt.Println("[selectorCache][run] begin run.")

		// exit early if already dead
		if c.quit() {
			return
		}

		// create new watcher
		w, err := c.so.Registry.Watch(
			registry.WatchService(name),
		)

		if err != nil {
			if c.quit() {
				return
			}
			log.Log(err)
			time.Sleep(time.Second)
			continue
		}

		// watch for events
		if err := c.watch(w); err != nil {
			if c.quit() {
				return
			}
			log.Log(err)
			continue
		}
	}


}

// watch loops the next event and calls update
// it returns if there's an error
func (c *selectorCache) watch(w registry.Watcher) error {
	quitLoop := make(chan interface{})
	defer func() {
		w.Stop()
		select {
		case quitLoop <- nil:
		default:
		}
	}()

	// manage this loop
	go func() {
		// wait for exit or reload signal
		select {
		case <-c.exit:
		case <-c.reload:
		case <-quitLoop:
		}
		// stop the watcher
		w.Stop()
	}()

	for {
		res, err := w.Next()
		if err != nil {
			return err
		}
		c.update(res)
	}
}

func (c *selectorCache) Init(opts ...selector.Option) error {
	for _, o := range opts {
		o(&c.so)
	}

	// reload the watcher
	go func() {
		select {
		case <-c.exit:
			return
		default:
			c.reload <- true
		}
	}()

	return nil
}

func (c *selectorCache) Options() selector.Options {
	return c.so
}

// Close stops the watcher and destroys the cache
func (c *selectorCache) Close() error {
	c.Lock()
	c.cache = make(map[string][]*registry.Service)
	c.watched = make(map[string]bool)
	c.Unlock()

	select {
	case <-c.exit:
		return nil
	default:
		close(c.exit)
	}
	return nil
}

func (c *selectorCache) String() string {
	return "cache"
}

func NewSelectorCache(opts ...selector.Option) *selectorCache {
	sopts := selector.Options{
		Strategy: selector.Random,
	}

	for _, opt := range opts {
		opt(&sopts)
	}

	if sopts.Registry == nil {
		sopts.Registry = registry.DefaultRegistry
	}

	ttl := DefaultTTL

	if sopts.Context != nil {
		if t, ok := sopts.Context.Value(ttlKey{}).(time.Duration); ok {
			ttl = t
		}
	}

	return &selectorCache{
		so:      sopts,
		ttl:     ttl,
		watched: make(map[string]bool),
		cache:   make(map[string][]*registry.Service),
		ttls:    make(map[string]time.Time),
		reload:  make(chan bool, 1),
		exit:    make(chan bool),
	}
}
