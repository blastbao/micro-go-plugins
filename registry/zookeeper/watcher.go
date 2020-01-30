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
	var wo registry.WatchOptions
	for _, o := range opts {
		o(&wo)
	}

	zw := &zookeeperWatcher{
		wo:      wo,
		client:  r.client,
		stop:    make(chan bool),
		results: make(chan result),
	}

	go zw.watch()
	return zw, nil
}

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

			// 只关注 "子节点变更" 事件，忽略其它事件，注：任何事件的发生都会导致 continue ，从而开始新的 for 循环
			if e.Type != zk.EventNodeChildrenChanged {
				continue
			}

			// 重新获取变更后的子节点列表
			newChildren, _, err := zw.client.Children(e.Path)
			if err != nil {
				respChan <- watchResponse{e, nil, err}
				return
			}


			// a node was added -- watch the new node

			// 遍历新的子节点列表
			for _, i := range newChildren {

				// 检查当前子节点是否是旧的节点，若是，则忽略
				if contains(children, i) {
					continue
				}

				// 如果当前节点是新增节点，则构造新节点的完整路径 newNode，以便于监听
				newNode := path.Join(e.Path, i)

				if key == prefix {

					// a new service was created under prefix
					go zw.watchDir(newNode, respChan)


					nodes, _, _ := zw.client.Children(newNode)
					for _, node := range nodes {
						n := path.Join(newNode, node)
						go zw.watchKey(n, respChan)
						s, _, err := zw.client.Get(n)
						e.Type = zk.EventNodeCreated

						srv, err := decode(s)
						if err != nil {
							continue
						}

						respChan <- watchResponse{
							event: e,			// 变更事件 zk.Event
							service: srv,		// 服务信息 registry.Service
							err: nil, 			// 错误信息
						}

					}
				} else {


					go zw.watchKey(newNode, respChan)


					s, _, err := zw.client.Get(newNode)
					e.Type = zk.EventNodeCreated

					srv, err := decode(s)
					if err != nil {
						continue
					}


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
//
//

func (zw *zookeeperWatcher) watchKey(key string, respChan chan watchResponse) {



	// 注意，这里 for 循环的目的是，由于 zk 中 watcher 是一次性的，当有事件触发 watcher 后，
	// 该 watch 即刻失效。因此，如果想不断监听某节点上的事件，需要在每次事件触发后重新添加 watcher 。

	for {

		// GetW returns the contents of a znode and sets a watch
		s, _, keyEventCh, err := zw.client.GetW(key)
		if err != nil {
			respChan <- watchResponse{zk.Event{}, nil, err}
			return
		}

		select {

		// 事件监听：
		// （1）对于 `数据变更`，`节点新增` 事件，需要重新获取变更后的节点内容 srv，并写入监听管道，然后继续 for 循环。
		// （2）对于 `节点删除` 事件，则无需（也无法）获取变更后节点内容，直接将事件写入监听管道，并 return 退出 for 循环。

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

func (zw *zookeeperWatcher) watch() {
	watchPath := prefix
	if len(zw.wo.Service) > 0 {
		watchPath = servicePath(zw.wo.Service)
	}

	//get all Services
	services, _, err := zw.client.Children(watchPath)
	if err != nil {
		zw.results <- result{nil, err}
	}
	respChan := make(chan watchResponse)

	//watch the prefix for new child nodes
	go zw.watchDir(watchPath, respChan)

	//watch every service
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
		zw.results <- result{&registry.Result{Action: action, Service: service}, nil}
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
