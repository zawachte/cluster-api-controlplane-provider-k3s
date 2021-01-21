module github.com/zawachte-msft/cluster-api-controlplane-provider-k3s

go 1.13

require (
	github.com/blang/semver v3.5.1+incompatible
	github.com/coredns/corefile-migration v1.0.11
	github.com/coreos/go-etcd v2.0.0+incompatible
	github.com/go-logr/logr v0.1.0
	github.com/onsi/ginkgo v1.12.1
	github.com/onsi/gomega v1.10.1
	github.com/pkg/errors v0.9.1
	github.com/zawachte-msft/cluster-api-bootstrap-provider-k3s v0.0.0-20210107204106-37f988104b18
	k8s.io/api v0.17.9
	k8s.io/apimachinery v0.17.9
	k8s.io/apiserver v0.17.9
	k8s.io/client-go v0.17.9
	k8s.io/klog v1.0.0
	k8s.io/utils v0.0.0-20200619165400-6e3d28b6ed19
	sigs.k8s.io/cluster-api v0.3.12
	sigs.k8s.io/controller-runtime v0.5.14
)
