package routing

// RouteEntry represents a single routing table entry.
type RouteEntry struct {
	Destination string
	Via         string
	Device      string
}

// NextHop describes a single nexthop in an ECMP route.
type NextHop struct {
	Via    string
	Dev    string
	Weight int
}
