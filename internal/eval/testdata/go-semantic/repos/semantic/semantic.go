package semantic

import "reflect"

type Runner interface {
	Run()
}

type AlsoRunner interface {
	Run()
}

type Base struct{}

func (Base) Promoted() {}

type Worker[T any] struct {
	Base
}

func (*Worker[T]) Run() {}

type Alias = Worker[int]

type Alternative struct{}

func (Alternative) Run() {}

type Left struct{}

func (Left) Touch() {}

type Right struct{}

func (Right) Touch() {}

func Generic[T any](T) {}

func helper() {}

func Dispatch(r Runner) {
	r.Run()
}

func Touch(left Left) {
	left.Touch()
}

func Dynamic(value any) {
	reflect.ValueOf(value).MethodByName("Run").Call(nil)
}

func Invoke(r Runner, worker *Worker[int], left Left) {
	helper()
	r.Run()
	worker.Promoted()
	method := worker.Run
	method()
	(*Worker[int]).Run(worker)
	var alias *Alias = worker
	alias.Run()
	left.Touch()
	Generic[int](1)
	reflect.ValueOf(worker).MethodByName("Run").Call(nil)
}
