module sigs.k8s.io/apiserver-network-proxy/konnectivity-client

go 1.17

// Prefer to keep requirements compatible with the oldest supported
// k/k minor version, to prevent client backport issues.
require (
	github.com/golang/protobuf v1.5.3
	google.golang.org/grpc v1.56.3
	k8s.io/klog/v2 v2.0.0
)

require go.uber.org/goleak v1.1.10

require (
	github.com/go-logr/logr v0.1.0 // indirect
	github.com/stretchr/testify v1.8.3 // indirect
	golang.org/x/lint v0.0.0-20210508222113-6edffad5e616 // indirect
	golang.org/x/net v0.9.0 // indirect
	golang.org/x/sys v0.7.0 // indirect
	golang.org/x/text v0.9.0 // indirect
	golang.org/x/tools v0.7.0 // indirect
	google.golang.org/genproto v0.0.0-20230410155749-daa745c078e1 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
)
