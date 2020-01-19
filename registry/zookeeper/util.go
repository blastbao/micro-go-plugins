package zookeeper

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/micro/go-micro/registry"
	"github.com/samuel/go-zookeeper/zk"
)

func encode(s *registry.Service) ([]byte, error) {
	return json.Marshal(s)
}

func decode(ds []byte) (*registry.Service, error) {
	var s *registry.Service
	err := json.Unmarshal(ds, &s)
	return s, err
}

// 把 服务名 和 节点ID 拼接成 zk 子路径，该路径代表一个可用节点
func nodePath(s, id string) string {

	// eg.
	//
	// service_name := "live/admin/test/id" => "live-admin-test-id"
	// node_id := "docker1/process2" 	=> "docker1-process2"
	// path := "live-admin-test-id/docker1-process2"

	service := strings.Replace(s, "/", "-", -1)
	node := strings.Replace(id, "/", "-", -1)
	return path.Join(prefix, service, node)
}

// parent + "/" + child
func childPath(parent, child string) string {
	return path.Join(parent, strings.Replace(child, "/", "-", -1))
}


// prefix + "/" + service_name
func servicePath(s string) string {
	return path.Join(prefix, strings.Replace(s, "/", "-", -1))
}


func createPath(path string, data []byte, client *zk.Conn) error {


	// 是否已经存在，若是则无需创建，直接返回
	exists, _, err := client.Exists(path)
	if err != nil {
		return err
	}

	if exists {
		return nil
	}

	// 逐级目录创建

	name := "/"
	p := strings.Split(path, "/")

	for _, v := range p[1 : len(p)-1] {
		name += v
		e, _, _ := client.Exists(name)
		if !e {
			_, err = client.Create(name, []byte{}, int32(0), zk.WorldACL(zk.PermAll))
			if err != nil {
				return err
			}
		}
		name += "/"
	}

	_, err = client.Create(path, data, int32(0), zk.WorldACL(zk.PermAll))
	return err
}

// 字符串包含检查
func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}
