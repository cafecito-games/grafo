package unavailable

import dependency "example.invalid/grafo-missing-module"

func Run() {
	dependency.Call()
}
