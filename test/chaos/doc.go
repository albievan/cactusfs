// Package chaos is the fault-injection suite. It runs the real cactusfs binary
// on six drives and damages what it has stored - corrupting and deleting
// shards, wiping drives, killing the process in the middle of writes - and
// checks the two things that must hold whatever happens: an object that was
// acknowledged is read back exactly, and nothing that is not the object is
// ever returned.
//
// The tests are compiled only with -tags=chaos and are run by "make
// test-chaos".
package chaos
