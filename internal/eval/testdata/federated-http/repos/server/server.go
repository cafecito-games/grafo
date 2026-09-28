package server

func Handler()     {}
func WrongMethod() {}
func AmbiguousA()  {}
func AmbiguousB()  {}

func Routes() {
	router.Get("/charge/{chargeID}", Handler)
	router.Post("/charge/{chargeID}", WrongMethod)
	router.Get("/ambiguous/{leftID}", AmbiguousA)
	router.Get("/ambiguous/{rightID}", AmbiguousB)
}
