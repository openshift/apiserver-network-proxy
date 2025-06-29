module sigs.k8s.io/apiserver-network-proxy/konnectivity-client

go 1.23.0

toolchain go1.23.9

// Prefer to keep requirements compatible with the oldest supported
// k/k minor version, to prevent client backport issues.
require (
	github.com/prometheus/client_golang v1.20.4
	go.uber.org/goleak v1.2.0
	golang.org/x/net v0.40.0 // indirect
	google.golang.org/grpc v1.56.3
	google.golang.org/protobuf v1.36.6
	k8s.io/klog/v2 v2.0.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v0.1.0 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.65.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.25.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
)
