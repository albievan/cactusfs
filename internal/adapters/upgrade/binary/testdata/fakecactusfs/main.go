// Command fakecactusfs stands for a build of cactusfs in the tests of the
// binary upgrader: "fakecactusfs version" answers as cactusfs does, with the
// version it was linked with, or behaves as it was told to. Both are given
// at link time as one value, -X main.fake=<version>/<behaviour>, where the
// behaviour is empty, hang, fail or other.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// fake is set with -ldflags -X when the test builds it.
var fake = "0.0.0/"

func main() {
	if len(os.Args) != 2 || os.Args[1] != "version" {
		os.Exit(2)
	}
	version, behaviour, _ := strings.Cut(fake, "/")
	switch behaviour {
	case "hang":
		time.Sleep(time.Hour)
	case "fail":
		os.Exit(3)
	case "other":
		fmt.Println("something else entirely")
		return
	}
	fmt.Printf("cactusfs %s local build (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
