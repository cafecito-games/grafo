package matrix

import (
	"os"
	"reflect"
)

func Run() string {
	helper()
	publish("jobs.ready")
	return os.Getenv("API_TOKEN")
}

func helper() {}

func Invoke(name string) {
	reflect.ValueOf(name).Call(nil)
}

func DynamicTarget() {}
