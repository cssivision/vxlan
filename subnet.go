package main

import (
	"fmt"

	"github.com/coreos/etcd/client"
)

type manager struct {
	cli client.KeysAPI
}

func newManager() manager {
	etcdCli, err := newEtcdClient()
	if err != nil {
		panic(fmt.Sprintf("new etcd client err: %v", err))
	}

	return manager{
		cli: etcdCli,
	}
}
