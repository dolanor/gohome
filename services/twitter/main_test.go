package twitter

import "github.com/barnybug/gohome/services"

func ExampleInterfaces() {
	var _ services.Service = (*TwitterService)(nil)
	// Output:
}
