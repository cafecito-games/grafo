package example.worker;

interface Job {
    String run(String payload);
}

class JavaWorker implements Job {
    public String run(String payload) {
        String token = System.getenv("JAVA_API_TOKEN");
        events.publish("java.jobs.ready");
        return payload;
    }
}
