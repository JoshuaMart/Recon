package fingerprint

// blockedPorts is Chromium's own restricted list, not a hand-written "non web" list. Chrome
// answers ERR_UNSAFE_PORT on these, so the failure is certain before the call: a fact about
// the instrument, where a hand-written list would be a hypothesis about the target, wrong
// about the forgotten application on 9000 this platform exists to find. On these services
// fingerprint_reachable stays undefined, correctly: there is no uncertainty about what a
// browser sees on a port it refuses to open.
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

// Renderable reports whether a browser will open this port at all. It is the whole
// baseline filter and reads transport reachability only, not outcome: an origin
// error behind a CDN counts as proof of death yet still deserves a baseline, as an
// edge answering for a dead origin is a page with something to read. Deriving it
// from the qualification would get that backwards.
func Renderable(port int) bool {
	_, blocked := blockedPorts[port]
	return !blocked
}
