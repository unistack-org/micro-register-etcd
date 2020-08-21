module github.com/unistack-org/micro-registry-etcd

go 1.15

replace (
	github.com/coreos/bbolt => go.etcd.io/bbolt v1.3.3
	github.com/coreos/etcd => github.com/ozonru/etcd v3.3.20-grpc1.27-origmodule+incompatible
	google.golang.org/grpc => google.golang.org/grpc v1.27.0
)

require (
	github.com/coreos/bbolt v1.3.2 // indirect
	github.com/coreos/etcd v3.3.18+incompatible
	github.com/grpc-ecosystem/grpc-gateway v1.9.0 // indirect
	github.com/mitchellh/hashstructure v1.0.0
	github.com/unistack-org/micro/v3 v3.0.0-20200821115321-c4a303190a68
	go.uber.org/zap v1.15.0
)
