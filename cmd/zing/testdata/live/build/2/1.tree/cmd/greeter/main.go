// Command greeter prints a personalized greeting.
package main

import (
	"fmt"

	"greeter"
)

func main() {
	fmt.Println(greeter.Greet("Ada"))
}
