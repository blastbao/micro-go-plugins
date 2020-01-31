package zookeeper

import (
	"errors"
	"path"




	"github.com/micro/go-micro/registry"
	"github.com/samuel/go-zookeeper/zk"
)




// zookeeperWatcher 实现了 micro.Watcher 接口，能够获取注册在 registry 中的服务（services）的更新信息。
//
//	type Watcher interface {
//		Next() (*Result, error)   	// Next() 是阻塞式的调用
//		Stop() 						// Stop()
//	}


type zookeeperWatcher struct {


	wo      registry.WatchOptions


	client  *zk.Conn


	// 退出信号
	stop    chan bool


	results chan result
}



type watchResponse struct {
	event   zk.Event
	service *registry.Service
	err     error
}

type result struct {
	res *registry.Result
	err error
}

func newZookeeperWatcher(r *zookeeperRegistry, opts ...registry.WatchOption) (registry.Watcher, error) {

	// 初始化配置项
	var wo registry.WatchOptions
	for _, o := range opts {
		o(&wo)
	}

	// 构造 zookeeperWatcher 结构体
	zw := &zookeeperWatcher{
		wo:      wo,
		client:  r.client,
		stop:    make(chan bool),
		results: make(chan result),
	}

	// 启动监视
	go zw.watch()


	return zw, nil
}




// 目录节点下存储了一组子节点
//
// 1. 获取目录节点 key 下所有子节点 children 并添加 watcher
// 2. 阻塞式监听事件到达
// 3. 如果事件发生，....


func (zw *zookeeperWatcher) watchDir(key string, respChan chan watchResponse) {


	// 注意，这里 for 循环的目的是，由于 zk 中 watcher 是一次性的，当有事件触发 watcher 后，
	// 该 watch 即刻失效。因此，如果想不断监听某节点上的事件，需要在每次事件触发后重新添加 watcher 。
	for {

		// 获取目录节点 key 下的 children, stats, 以及用于监听目录下子节点变更事件的 childEventCh 管道。
		children, _, childEventCh, err := zw.client.ChildrenW(key)	// 注：zk.ChildrenW() 相当于调用了 zk.Children() 和 zk.addWatcher()。
		if err != nil {
			// 若报错，意味着无法读取 key 的 children，返回错误信息 err
			respChan <- watchResponse{zk.Event{}, nil, err}
			return
		}


		select {

		// 监听事件（阻塞式）
		case e := <-childEventCh:

			// 只关注 "子节点变更" 事件，忽略其它事件，注：任何事件的发生都会导致 watcher 失效 ，需要开始新的循环来添加 watcher，所以这里调用 continue
			if e.Type != zk.EventNodeChildrenChanged {
				continue
			}

			// 重新获取变更后的子节点列表
			newChildren, _, err := zw.client.Children(e.Path)
			if err != nil {
				respChan <- watchResponse{e, nil, err}
				return
			}

			// 检查是否有新增节点，若果有，则 watch 它，否则忽略

			// 遍历新的子节点列表
			for _, i := range newChildren {

				// 检查当前子节点是否是旧的节点，若是，则忽略
				if contains(children, i) {
					continue
				}


				// 至此，当前子节点 child 一定是新增节点，
				//（1）对于 "key == prefix" 情况，新增节点意味着有新注册的服务
				//（2）对于 "key != prefix" 情况，新增节点意味着某服务下有新增加的节点信息(ip+port)


				// 构造节点的完整路径 newNode
				newNode := path.Join(e.Path, i)


				// 如果 key 是根目录(prefix)，则其 children 仍旧是目录节点，每个 child 对应了一个独立的服务，
				// 例如在 "services/livechat-room-test-ph" 中，prefix 是 "services"，"livechat-room-test-ph" 是一个 child 。
				//
				// 此时，不仅需要监听 children 目录节点，对于 child 下的每个内容节点，也需要监听。

				if key == prefix {


					// 递归调用 zw.watchDir()，在该调用中会走到下面的 else 分支，
					//
					// a new service was created under prefix
					go zw.watchDir(newNode, respChan)


					// 获取 child 下的所有内容节点，逐个监听、解析、发送...
					nodes, _, _ := zw.client.Children(newNode)
					for _, node := range nodes {

						// 监听
						n := path.Join(newNode, node)
						go zw.watchKey(n, respChan)

						// 反序列化
						s, _, err := zw.client.Get(n)
						e.Type = zk.EventNodeCreated

						srv, err := decode(s)
						if err != nil {
							continue
						}

						// 发送
						respChan <- watchResponse{
							event: e,			// 变更事件 zk.Event
							service: srv,		// 服务信息 registry.Service
							err: nil, 			// 错误信息
						}
					}


				// 如果 key 不是根目录(prefix)，则其 children 是内容节点，每个 child 对应了一个服务节点信息(end_point)，
				// 这里，需要对每个 child 进行监听、解析、发送...
				} else {

					// 监听
					go zw.watchKey(newNode, respChan)

					// 解析
					s, _, err := zw.client.Get(newNode)
					e.Type = zk.EventNodeCreated

					srv, err := decode(s)
					if err != nil {
						continue
					}

					// 发送
					respChan <- watchResponse{
						event: e,			// 变更事件 zk.Event
						service: srv,		// 服务信息 registry.Service
						err: nil, 			// 错误信息
					}

				}
			}
		case <-zw.stop:
			// There is no way to stop GetW/ChildrenW so just quit
			return
		}
	}
}




// 内容节点存储了 registry.Service 结构体序列化后的字节序列。
//
// 1. 获取节点内容 s 并添加 watcher
// 2. 阻塞式监听事件到达
// 3. 如果事件发生，将 事件信息 和 节点内容 发送到监听管道中，并重新添加 watcher 进行监听
// 4. 如果发生了 节点删除事件、stop信号，则退出监听

func (zw *zookeeperWatcher) watchKey(key string, respChan chan watchResponse) {

	// 注意，这里 for 循环的目的是，由于 zk 中 watcher 是一次性的，当有事件触发 watcher 后，
	// 该 watch 即刻失效。因此，如果想不断监听某节点上的事件，需要在每次事件触发后重新添加 watcher 。

	for {

		// 1. 获取节点内容 s 并添加 watcher
		// GetW returns the contents of a znode and sets a watch
		s, _, keyEventCh, err := zw.client.GetW(key)
		if err != nil {
			respChan <- watchResponse{zk.Event{}, nil, err}
			return
		}

		// 2. 阻塞式监听事件到达
		select {

		// 事件监听：
		// （1）对于 `数据变更`，`节点新增` 事件，需要重新获取变更后的节点内容 srv，并将事件写入监听管道，然后继续 for 循环进行事件监听。
		// （2）对于 `节点删除` 事件，则无需（也无法）获取变更后节点内容，直接将事件写入监听管道，并 return 退出监听。

		case e := <-keyEventCh:

			switch e.Type {
			case zk.EventNodeDataChanged, zk.EventNodeCreated, zk.EventNodeDeleted:

				// 如果节点被删除，则调用 zk.Get(path) 会报错，所以需要检查一下；
				// 否则，就调用 zk.Get(path) 获取节点最新内容，存入 s []byte 中。
				if e.Type != zk.EventNodeDeleted {
					// get the updated service
					s, _, err = zw.client.Get(e.Path)
				}

				// 从 s 反序列化解析出 registry.Service 对象。
				srv, err := decode(s)
				if err != nil {
					continue
				}

				// 把事件和服务信息写入 watch 管道中。
				respChan <- watchResponse{
					event: e,			// 变更事件 zk.Event
					service: srv,		// 服务信息 registry.Service
					err: nil, 			// 错误信息
				}
			}

			if e.Type == zk.EventNodeDeleted {
				//The Node was deleted - stop watching
				return
			}

		// 退出信号监听
		case <-zw.stop:
			// There is no way to stop GetW/ChildrenW so just quit
			return
		}

	}
}


func (zw *zookeeperWatcher) watchShopee() {


	// 1. 确定需 watch 的 service(s)
	//	(1) 如果 option 中指定了 service，就只 watch 它
	// 	(2) 如果 option 未指定 service，就从 prefix 中拉取全部 services，并全部 watch 它们
	var services []string
	if len(zw.wo.Service) > 0 {
		services = []string{zw.wo.Service}
	} else {
		allServices, _, err := zw.client.Children(prefix)
		if err != nil {
			zw.results <- result{nil, err}
		}
		services = allServices
	}

	// 2. 每个待 watch 的 service 都是 zk 中的目录节点，其子节点是一个个以 ip:port 命名的内容节点。
	//	（1）调用 watchDir 来监视每个 service 目录节点
	//	（2）调用 watchKey 来监视每个 service 目录下的各个内容节点
	//	（3）所有的节点变更信息会通过管道 respChan 汇总起来
	respChan := make(chan watchResponse)
	//watch every service
	for _, service := range services {
		//sPath := servicePath(service)
		sPath := childPath(prefix, service)
		go zw.watchDir(sPath, respChan)
		children, _, err := zw.client.Children(sPath)
		if err != nil {
			zw.results <- result{nil, err}
		}
		for _, c := range children {
			go zw.watchKey(path.Join(sPath, c), respChan)
		}
	}


	var service *registry.Service
	var action string
	for {
		// 阻塞式等待数据
		select {
		case <-zw.stop:
			return
		case rsp := <-respChan:
			if rsp.err != nil {
				zw.results <- result{nil, rsp.err}
				continue
			}
			switch rsp.event.Type {
			case zk.EventNodeDataChanged:
				action = "update"
				service = rsp.service
			case zk.EventNodeDeleted:
				action = "delete"
				service = rsp.service
			case zk.EventNodeCreated:
				action = "create"
				service = rsp.service
			}
		}


		zw.results <- result{
			res: &registry.Result{
				Action: action,
				Service: service,
			},
			err: nil,
		}
	}
}


// 注：原生的 watch 函数不适用于 shopee zk 的目录结构。

func (zw *zookeeperWatcher) watch() {
	watchPath := prefix
	if len(zw.wo.Service) > 0 {
		watchPath = servicePath(zw.wo.Service)
	}

	// get all services
	services, _, err := zw.client.Children(watchPath)
	if err != nil {
		zw.results <- result{nil, err}
	}

	respChan := make(chan watchResponse)

	// watch the prefix for new child nodes
	go zw.watchDir(watchPath, respChan)

	// watch every service
	for _, service := range services {

		sPath := childPath(watchPath, service)
		go zw.watchDir(sPath, respChan)

		children, _, err := zw.client.Children(sPath)
		if err != nil {
			zw.results <- result{nil, err}
		}

		for _, c := range children {
			go zw.watchKey(path.Join(sPath, c), respChan)
		}

	}

	var service *registry.Service
	var action string

	for {

		select {
		case <-zw.stop:
			return
		case rsp := <-respChan:
			if rsp.err != nil {
				zw.results <- result{nil, err}
				continue
			}
			switch rsp.event.Type {
			case zk.EventNodeDataChanged:
				action = "update"
				service = rsp.service
			case zk.EventNodeDeleted:
				action = "delete"
				service = rsp.service
			case zk.EventNodeCreated:
				action = "create"
				service = rsp.service
			}
		}
		zw.results <- result{
			res: &registry.Result{
				Action: action,
				Service: service,
			},
			err: nil,
		}
	}
}


func (zw *zookeeperWatcher) Stop() {
	select {
	case <-zw.stop:
		return
	default:
		close(zw.stop)
	}
}

func (zw *zookeeperWatcher) Next() (*registry.Result, error) {
	select {
	case <-zw.stop:
		return nil, errors.New("watcher stopped")
	case r := <-zw.results:
		return r.res, r.err
	}
}
