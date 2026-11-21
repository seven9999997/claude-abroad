module claude-abroad

go 1.26.10

// gvisor-tap-vsock 使用 third_party/ 中打过 Cage 补丁的源码（见 patches/gvisor-tap-vsock）。
replace github.com/containers/gvisor-tap-vsock => ./third_party/gvisor-tap-vsock

require (
	github.com/Code-Hex/vz/v3 v3.8.0
	github.com/hashicorp/yamux v0.1.2
	github.com/miekg/dns v1.1.73
	github.com/sirupsen/logrus v1.9.4
	go.yaml.in/yaml/v3 v3.0.6
	golang.org/x/net v0.62.0
	golang.org/x/sys v0.50.0
	gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/apparentlymart/go-cidr v1.1.1 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/inetaf/tcpproxy v0.0.0-20250222171855-c4b9df066048 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20240710054256-ddd8a41251c9 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/time v0.5.0 // indirect
)

require (
	github.com/Code-Hex/go-infinity-channel v1.0.0 // indirect
	github.com/containers/gvisor-tap-vsock v0.0.0-00010101000000-000000000000
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
