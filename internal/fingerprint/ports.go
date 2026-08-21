package fingerprint

// blockedPorts is Chromium's own restricted list.
//
// It is the browser's list rather than a hand written list of "non web" ports,
// and the difference is the whole point. Chrome answers ERR_UNSAFE_PORT on
// these, so the failure is certain before the call, which makes it a fact about
// the instrument rather than a hypothesis about the target. A hand written list
// would be the hypothesis, and it would be wrong about the forgotten
// application on 9000 that is exactly what this platform exists to find.
//
// The consequence is assumed: on these services fingerprint_reachable stays
// undefined. That is correct. Unobservable qualifies an asset whose state is
// unknown, and there is no uncertainty about what a browser sees on a port it
// refuses to open.
var blockedPorts = map[int]struct{}{}

func init() {
	list := []int{
		1, 7, 9, 11, 13, 15, 17, 19, 20, 21, 22, 23, 25, 37, 42, 43, 53, 69, 77,
		79, 87, 95, 101, 102, 103, 104, 109, 110, 111, 113, 115, 117, 119, 123,
		135, 137, 138, 139, 143, 161, 179, 389, 427, 465, 512, 513, 514, 515,
		526, 530, 531, 532, 540, 548, 554, 556, 563, 587, 601, 636, 989, 990,
		993, 995, 1719, 1720, 1723, 2049, 3659, 4045, 4190, 5060, 5061, 6000,
		6566, 6665, 6666, 6667, 6668, 6669, 6697, 10080,
	}
	for _, port := range list {
		blockedPorts[port] = struct{}{}
	}
}

// Renderable reports whether a browser will open this port at all.
//
// It is the whole of the baseline filter, and it reads transport reachability
// and nothing else. It is deliberately not a filter on outcome: an origin error
// behind a CDN is an informative failure counted as proof of death, and it
// still deserves a baseline, because an edge answering for a dead origin is a
// page with something to read. Deriving this from the qualification would get
// that case exactly backwards.
func Renderable(port int) bool {
	_, blocked := blockedPorts[port]
	return !blocked
}
