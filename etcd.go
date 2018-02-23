package main

import (
	"time"

	"github.com/coreos/etcd/client"
)

func newEtcdClient(cfg config) (client.KeysAPI, error) {
	etcdCfg := client.Config{
		Endpoints: []string{cfg.etcdEndpoint},
		Transport: client.DefaultTransport,
		// set timeout per request to fail fast when the target endpoint is unavailable
		HeaderTimeoutPerRequest: time.Second,
	}
	c, err := client.New(etcdCfg)
	if err != nil {
		return nil, err
	}
	return client.NewKeysAPI(c), nil
}
