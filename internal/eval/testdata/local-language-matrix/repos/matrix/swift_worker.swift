protocol SwiftRunnable {
    func run(value: String) -> String
}

struct SwiftWorker: SwiftRunnable {
    func run(value: String) -> String {
        let result = value
        events.publish("swift.ready")
        return result
    }
}

func invokeSwift() {
    let worker: SwiftWorker = SwiftWorker()
    let input = "ready"
    worker.run(value: input)
}
