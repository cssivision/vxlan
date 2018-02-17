# The remote IP should be 100.1.1.1/100.1.1.2. This should work:
ip address add 100.1.1.1/24 dev enp0s8
ifconfig enp0s8 up
ip link add name vxlan42 type vxlan id 42 dev enp0s8 remote 100.1.1.2 local 100.1.1.1 dstport 4789
ip address add 50.1.1.1/24 dev vxlan42
ip link set up vxlan42

ip address add 100.1.1.2/24 dev enp0s8
ifconfig enp0s8 up
ip link add name vxlan42 type vxlan id 42 dev enp0s8 remote 100.1.1.1 local 100.1.1.2 dstport 4789
ip address add 50.1.1.2/24 dev vxlan42
ip link set up vxlan42