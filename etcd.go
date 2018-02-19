package main

import (
	"time"

	"github.com/coreos/etcd/client"
)

func newEtcdClient() (client.KeysAPI, error) {
	cfg := client.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
		Transport: client.DefaultTransport,
		// set timeout per request to fail fast when the target endpoint is unavailable
		HeaderTimeoutPerRequest: time.Second,
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	return client.NewKeysAPI(c), nil
}
