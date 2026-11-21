package envcheck

import (
	"net/netip"
	"strconv"
)

type netipAddr = netip.Addr

func strconvItoa(n int) string { return strconv.Itoa(n) }
