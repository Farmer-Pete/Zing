// Package greeter produces greetings and farewells.
package greeter

// Greet returns a personalized greeting for name.
func Greet(name string) string {
	return "Hello, " + name + "!"
}

// Farewell returns a personalized farewell for name.
func Farewell(name string) string {
	return "Goodbye, " + name + "."
}
