package quote

import "fmt"

// This file is the test fixture for Gitaly linters

func quoteOkay() {
	fmt.Printf("hello world: %q", "today is a good day")
	fmt.Printf("hello world: %d", 123)
	fmt.Printf("%s something", "hello")
	fmt.Printf("hello world: %s", "this is good")
	fmt.Printf("hello world: %s something", "this is good")
}

func quoteNotOkay() {
	fmt.Printf("hello world: '%s'", "today is a good day") //  want "wrapping %s verb with quotes is not encouraged, please use %q instead"
	fmt.Printf(`hello world: "%s"`, "today is a good day") // want "wrapping %s verb with quotes is not encouraged, please use %q instead"
	fmt.Printf(`hello world: "%s"`, "today is a good day") // want "wrapping %s verb with quotes is not encouraged, please use %q instead"
	str := `so is
tomorrow`
	fmt.Printf("hello world: '%s'", str) // want "wrapping %s verb with quotes is not encouraged, please use %q instead"
	fmt.Printf(`hello world: "%s"`, str) // want "wrapping %s verb with quotes is not encouraged, please use %q instead"
	fmt.Printf(`hello world: "%s"`, str) // want "wrapping %s verb with quotes is not encouraged, please use %q instead"

	fmt.Printf("hello world:%s %s '%s'", "today", "is a", "good day") // want "wrapping %s verb with quotes is not encouraged, please use %q instead"
	fmt.Printf("hello world: %d '%s'", 123, "today is a good day")    // want "wrapping %s verb with quotes is not encouraged, please use %q instead"
}
