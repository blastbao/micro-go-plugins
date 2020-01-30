// Package zookeeper provides a zookeeper registry
package zookeeper

import (
	"errors"
	"sync"
	"time"

	"github.com/micro/go-log"
	"github.com/micro/go-micro/config/cmd"
	"github.com/micro/go-micro/registry"
	"github.com/samuel/go-zookeeper/zk"

	hash "github.com/mitchellh/hashstructure"
)

var (
	prefix = "/micro-registry"
)

// zookeeperRegistry 实现了 micro.Registry 接口，能够提供服务发现能力。
//
//	type Registry interface {
//
//		Init(...Option) error
//
//		Options() Options
//
//		Register(*Service, ...RegisterOption) error
//
//		Deregister(*Service) error
//
//		GetService(string) ([]*Service, error)
//
//		ListServices() ([]*Service, error)
//
//		Watch(...WatchOption) (Watcher, error)
//
//		String() string
//
//	}


type zookeeperRegistry struct {

	// zk 连接
	client  *zk.Conn

	// 配置项：zk addresses, dial timeout, others
	options registry.Options

	// 锁用于保护 register
	sync.Mutex

	// 保存了 < 服务名，hash 值 >
	register map[string]uint64
}

func init() {
	cmd.DefaultRegistries["zookeeper"] = NewRegistry
}

// configure() 函数的功能和 NewRegistry() 基本一致，它用来确保当前 zk 连接有效，同时会更新 z.options 中的一些配置。
//
// 1. 检查超时配置是否为空，若是则置为默认值 5
// 2. 检查 zkConn 是否为 nil, 若是则需重连
// 3. 检查 zk 地址列表是否有变更，若是则需重连
// 4. 执行重连（类似于 NewRegistry 函数的行为）
func configure(z *zookeeperRegistry, opts ...registry.Option) error {

	cAddrs := z.options.Addrs

	for _, o := range opts {
		o(&z.options)
	}

	if z.options.Timeout == 0 {
		z.options.Timeout = 5
	}

	// already set
	if z.client != nil && len(z.options.Addrs) == len(cAddrs) {
		return nil
	}

	// reset
	cAddrs = nil

	for _, addr := range z.options.Addrs {
		if len(addr) == 0 {
			continue
		}
		cAddrs = append(cAddrs, addr)
	}

	if len(cAddrs) == 0 {
		cAddrs = []string{"127.0.0.1:2181"}
	}



	// connect to zookeeper
	c, _, err := zk.Connect(cAddrs, time.Second*z.options.Timeout)
	if err != nil {
		log.Fatal(err)
	}

	// create our prefix path
	if err := createPath(prefix, []byte{}, c); err != nil {
		log.Fatal(err)
	}

	z.client = c
	return nil
}



func (z *zookeeperRegistry) Init(opts ...registry.Option) error {
	// configure() 函数用来确保当前 zk 连接有效，同时更新 z.options 中的一些配置。
	return configure(z, opts...)
}


func (z *zookeeperRegistry) Options() registry.Options {
	return z.options
}


// 注销服务
func (z *zookeeperRegistry) Deregister(s *registry.Service) error {

	if len(s.Nodes) == 0 {
		return errors.New("require at least one node")
	}

	// delete our hash of the service
	z.Lock()
	delete(z.register, s.Name)
	z.Unlock()

	// 遍历 zk 中 s.Name 路径下的所有节点，逐个清除。
	for _, node := range s.Nodes {
		// 从 zk 中删除目标节点。
		err := z.client.Delete(nodePath(s.Name, node.Id), -1) // nodePath(s.Name, node.Id) 把服务名 s.Name 和节点ID node.Id 拼接成 zk 子路径，该路径代表一个可用节点。
		if err != nil {
			return err
		}
	}

	return nil
}




// 注册服务

// 计算 s 的 hash 值，记为 h
// 检查 s 是否已经注册，若已经注册且hash值未发生改变，则已经注册，直接返回
// 将 h 添加到 z.register[s.Name] 里保存起来
//
//
//
//
func (z *zookeeperRegistry) Register(s *registry.Service, opts ...registry.RegisterOption) error {

	if len(s.Nodes) == 0 {
		return errors.New("Require at least one node")
	}

	var options registry.RegisterOptions
	for _, o := range opts {
		o(&options)
	}


	// 1

	// create hash of service; uint64
	h, err := hash.Hash(s, nil)
	if err != nil {
		return err
	}

	// 2

	// get existing hash
	z.Lock()
	v, ok := z.register[s.Name]
	z.Unlock()

	// the service is unchanged, skip registering
	if ok && v == h {
		return nil
	}

	// 拷贝 s 为 service
	service := &registry.Service{
		Name:      s.Name,
		Version:   s.Version,
		Metadata:  s.Metadata,
		Endpoints: s.Endpoints,
	}

	// 遍历 s.Nodes ，若存在则更新内容，若不存在则创建
	for _, node := range s.Nodes {

		service.Nodes = []*registry.Node{node}


		// 检查 "prefix/server-name/node-id" 是否已经存在
		exists, _, err := z.client.Exists(nodePath(service.Name, node.Id))
		if err != nil {
			return err
		}

		// 序列化 service 为 data
		srv, err := encode(service)
		if err != nil {
			return err
		}

		// 若 "prefix/server-name/node-id" 已经存在，则设置（Set）该路径存储数据为 `srv`
		if exists {
			_, err := z.client.Set(nodePath(service.Name, node.Id), srv, -1)
			if err != nil {
				return err
			}

		// 否则，创建 "prefix/server-name/node-id" 路径同时保存数据为 `srv`
		} else {
			err := createPath(nodePath(service.Name, node.Id), srv, z.client)
			if err != nil {
				return err
			}
		}

	}

	// 注册 hash 值到 map 中

	// save our hash of the service
	z.Lock()
	z.register[s.Name] = h
	z.Unlock()

	return nil
}

func (z *zookeeperRegistry) GetService(name string) ([]*registry.Service, error) {
	l, _, err := z.client.Children(servicePath(name))
	if err != nil {
		return nil, err
	}

	serviceMap := make(map[string]*registry.Service)

	for _, n := range l {
		_, stat, err := z.client.Children(nodePath(name, n))
		if err != nil {
			return nil, err
		}

		if stat.NumChildren > 0 {
			continue
		}

		b, _, err := z.client.Get(nodePath(name, n))
		if err != nil {
			return nil, err
		}

		sn, err := decode(b)
		if err != nil {
			return nil, err
		}

		s, ok := serviceMap[sn.Version]
		if !ok {
			s = &registry.Service{
				Name:      sn.Name,
				Version:   sn.Version,
				Metadata:  sn.Metadata,
				Endpoints: sn.Endpoints,
			}
			serviceMap[s.Version] = s
		}

		for _, node := range sn.Nodes {
			s.Nodes = append(s.Nodes, node)
		}
	}

	var services []*registry.Service

	for _, service := range serviceMap {
		services = append(services, service)
	}

	return services, nil
}

func (z *zookeeperRegistry) ListServices() ([]*registry.Service, error) {

	srv, _, err := z.client.Children(prefix)
	if err != nil {
		return nil, err
	}

	serviceMap := make(map[string]*registry.Service)

	for _, key := range srv {
		s := servicePath(key)
		nodes, _, err := z.client.Children(s)
		if err != nil {
			return nil, err
		}

		for _, node := range nodes {
			_, stat, err := z.client.Children(nodePath(key, node))
			if err != nil {
				return nil, err
			}

			if stat.NumChildren == 0 {
				b, _, err := z.client.Get(nodePath(key, node))
				if err != nil {
					return nil, err
				}
				i, err := decode(b)
				if err != nil {
					return nil, err
				}
				serviceMap[s] = &registry.Service{Name: i.Name}
			}
		}
	}

	var services []*registry.Service

	for _, service := range serviceMap {
		services = append(services, service)
	}

	return services, nil
}

func (z *zookeeperRegistry) String() string {
	return "zookeeper"
}

func (z *zookeeperRegistry) Watch(opts ...registry.WatchOption) (registry.Watcher, error) {
	return newZookeeperWatcher(z, opts...)
}

// 流程：
// 	初始化 addrs、connect timeout
// 	建立 zk.Conn
// 	创建 zk 根目录，该目录的完整路径保存在变量 prefix 中
// 	创建&初始化 zookeeperRegistry 结构体对象，返回对象指针

func NewRegistry(opts ...registry.Option) registry.Registry {
	var options registry.Options
	for _, o := range opts {
		o(&options)
	}

	if options.Timeout == 0 {
		options.Timeout = 5
	}

	var cAddrs []string
	for _, addr := range options.Addrs {
		if len(addr) == 0 {
			continue
		}
		cAddrs = append(cAddrs, addr)
	}

	if len(cAddrs) == 0 {
		cAddrs = []string{"127.0.0.1:2181"}
	}

	// connect to zookeeper
	c, _, err := zk.Connect(cAddrs, time.Second*options.Timeout)
	if err != nil {
		log.Fatal(err)
	}

	// create our prefix path
	if err := createPath(prefix, []byte{}, c); err != nil {
		log.Fatal(err)
	}

	return &zookeeperRegistry{
		client:   c,
		options:  options,
		register: make(map[string]uint64),
	}
}
