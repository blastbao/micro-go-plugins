// Package cache is a caching selector. It uses the registry watcher.
package selector

import (
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


// 检查 selectorCache.watch() 函数是否已经退出，若已退出返回 true。
func (c *selectorCache) quit() bool {

	// 非阻塞式调用，若 c.exit 管道 `不可读` 或者 `已关闭`，则 select 会走到 default 分支。
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

// 根据 res.action 和 res.service 更新本地缓存
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

	// 根据 res.Action 决定如何更新缓存
	switch res.Action {
	case "create", "update":

		// 如果 "service == nil" ，意味着 cache 中没有匹配 res.Service.Version 的 s * registry.Service ，
		// 此时需要在 cache 中添加新增的 res.Service 信息。
		if service == nil {
			c.set(res.Service.Name, append(services, res.Service))
			return
		}

		// 如果 "service != nil" ，则需要更新 cache 中的对应项。

		// append old nodes to new service

		// 二重循环：遍历缓存中的旧的 Nodes，逐个检查当前 node 是否存在于 res.Service.Nodes 中，
		//（1）若存在，则无需添加，直接忽略；
		//（2）若不存在，则需要添加到 res.Service.Nodes 中
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

		// 更新缓存
		services[index] = res.Service
		c.set(res.Service.Name, services)


	case "delete":

		// 如果 "service == nil" ，意味着 cache 中没有匹配 res.Service.Version 的 s * registry.Service ，无需删除
		if service == nil {
			return
		}

		// 如果 "service != nil" ，则需要删除 cache 中的某些 nodes 。




		var nodes []*registry.Node

		// filter cur nodes to remove the dead one
		//

		// 二重循环：遍历缓存中的旧的 service.Nodes，逐个检查当前 node 是否存在于 res.Service.Nodes 中，
		//（1）若不存在，则无需删除，临时添加到 nodes 中暂存起来；
		//（2）若存在，需要删除，不能暂存，直接忽略。
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
		//
		// 如果存在未删除的 nodes ，需要将这些 nodes 保留在缓存中。
		if len(nodes) > 0 {
			service.Nodes = nodes
			services[index] = service
			c.set(service.Name, services)
			return
		}

		// 如果所有 nodes 都被删除了，就直接删除缓存项即可。
		// 下面的操作是在 services[] 中删除当前 service 对应项，如果只有一项，就删除整个缓存。

		// zero nodes left

		// only have one thing to delete nuke the thing
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

		// 1. 检查 selectorCache.watch() 函数是否已经退出，若已退出则 c.quit() 返回 true，这里便直接 return 。
		//
		// exit early if already dead
		if c.quit() {
			return
		}

		// 2. 创建一个 zookeeperWatcher 对象 w ，它实现了 micro.Watcher 接口，能够获取注册在 registry 中的服务的zk 节点变更信息，
		// 所有变更信息可以通过 micro.Watcher 接口的 Next() 来获取，可被用于更新本地缓存信息。
		//
		//	type micro.Watcher interface {
		//		Next() (*Result, error)   	// Next() 是阻塞式的调用
		//		Stop() 						// Stop()
		//	}
		//
		// create new watcher
		w, err := c.so.Registry.Watch(
			registry.WatchService(name),
		)

		// 3. 如果 Watch() 出错，检查是否需要退出或者重试。
		if err != nil {

			// 检查 selectorCache.watch() 协程是否已经退出，若已退出则 c.quit() 返回 true，这里便直接 return 。
			if c.quit() {
				return
			}

			log.Log(err)
			time.Sleep(time.Second)
			continue
		}


		// 4. 如果 c.so.Registry.Watch() 成功，则启动 c.watch(w) 函数，
		// 该函数内部会不断的调用 w.Next() 获取服务节点的变更信息，并根据这些变更信息来更新本地缓存；
		//
		// 注意，c.watch(w) 是同步的调用，当收到 c.exit、c.reload 信号，或者 w.Next() 返回 error，c.watch(w) 会退出并返回错误。
		//
		//
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
//
// 循环不断的调用 w.Next 获取服务节点的变更信息，根据这些变更信息来更新本地缓存；
// 如果收到了 退出信号 或者 Next() 调用返回错误，就调用 watcher 的 stop 函数，并 return。

func (c *selectorCache) watch(w registry.Watcher) error {

	// 用于控制匿名 goroutine 的退出
	quit := make(chan interface{})

	// watch 函数退出前，defer 会调用 Stop() 且尝试发送信号给 quit 管道，其能够导致后面 goroutine 退出
	defer func() {
		w.Stop()
		select {
		case quit <- nil:
		default:
		}
	}()

	// 这个 goroutine 会一直卡在 select 上，直到收到 exit、quit 或者 reload 信号，会调用 w.Stop() 函数，
	// 在调用 w.Stop() 之后，后面的 Next() 函数就会返回 error ，从而结束 for 循环并返回。
	go func() {
		select {
		case <-c.exit:
		// 除非 Init() 函数被调用，否则 reload 管道中不会收到信号
		case <-c.reload:
		case <-quit:
		}
		w.Stop()
	}()

	// 主流程
	for {

		// 阻塞式调用 Next() 函数，返回 res *registry.Result，其包含 res.action 和 res.service.Nodes 等信息；
		// 在调用 stop() 之后，后序的 Next() 函数就会返回 error，进而导致 for loop 退出。
		res, err := w.Next()
		if err != nil {
			return err
		}

		// 根据 res.action 和 res.service 更新本地缓存
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
//
// Close 函数清空 cache、watched，且发出 exit signal 以停止 watch 函数的运行
func (c *selectorCache) Close() error {
	c.Lock()
	c.cache = make(map[string][]*registry.Service)
	c.watched = make(map[string]bool)
	c.Unlock()

	select {
	case <-c.exit:
		return nil
	default:
		// 发送 exit 信号，watch() 会接收到并退出
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
