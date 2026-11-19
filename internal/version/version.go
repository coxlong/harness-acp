package version

import "fmt"

const Major = 0
const Minor = 1
const Patch = 0

var Version = fmt.Sprintf("%d.%d.%d", Major, Minor, Patch)

func String() string { return "harnessd v" + Version }
