package screen

// macToRemote maps a point of the macOS viewer's view (vw×vh points, y
// from the top) to the remote picture's pixels: the picture (fw×fh) is
// fitted and centred (CALayer's resizeAspect), and points on the bars
// clamp to its edge. False before there is a picture.
func macToRemote(x, y, vw, vh float64, fw, fh int) (int, int, bool) {
	if vw <= 0 || vh <= 0 || fw <= 0 || fh <= 0 {
		return 0, 0, false
	}
	s := min(vw/float64(fw), vh/float64(fh))
	dx, dy := (vw-float64(fw)*s)/2, (vh-float64(fh)*s)/2
	rx, ry := int((x-dx)/s), int((y-dy)/s)
	return min(max(rx, 0), fw-1), min(max(ry, 0), fh-1), true
}
