CREATE TABLE public.events (
  id bigint PRIMARY KEY,
  payload text NOT NULL
);

CREATE VIEW public.recent_events AS
SELECT id, payload FROM public.events;
