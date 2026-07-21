package main

import "fmt"

// greet returns a fixed greeting. Its contract (name + return shape) must
// be preserved by any fix — the semantic assertion checks for it.
func greet(name string) string {
	return "hello, " + name
}

func main() {
	// Deliberate build error: `who` is undefined. The minimal fix is to
	// declare it (e.g. who := "world") — NOT to rewrite greet or main.
	fmt.Println(greet(who))
}
