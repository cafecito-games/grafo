package server

func Handler() {}
func WrongMethod() {}

func Routes() {
	router.Get("/charge/{chargeID}", Handler)
	router.Post("/charge/{chargeID}", WrongMethod)
}
