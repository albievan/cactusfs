package s3client

// SetChunkSize sets the payload of a chunk of c, so that a test sends a body
// of a few KiB as several chunks.
func SetChunkSize(c *Client, n int) {
	c.chunkSize = n
}

// SetUploadsPage sets how many uploads a page of c's listings asks for, so
// that a test lists a few uploads over several pages.
func SetUploadsPage(c *Client, n int) {
	c.uploadsPage = n
}

// SetPartsPage sets how many parts a page of c's listings of parts asks
// for, so that a test lists a few parts over several pages.
func SetPartsPage(c *Client, n int) {
	c.partsPage = n
}
