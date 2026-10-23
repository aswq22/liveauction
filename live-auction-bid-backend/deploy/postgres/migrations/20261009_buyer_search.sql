-- Run explicitly with psql -v ON_ERROR_STOP=1 -f this file.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
DO $$ BEGIN
 IF string_to_array((SELECT extversion FROM pg_extension WHERE extname='vector'),'.')::int[] < ARRAY[0,8,0] THEN
  RAISE EXCEPTION 'buyer search requires pgvector >= 0.8.0';
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS buyer_search_generations(
 id varchar(64) PRIMARY KEY,
 table_name varchar(80) NOT NULL UNIQUE,
 model text NOT NULL,
 provider_version text NOT NULL,
 template_version text NOT NULL,
 dimensions integer NOT NULL CHECK(dimensions BETWEEN 1 AND 2000),
 max_distance double precision NOT NULL CHECK(max_distance>0 AND max_distance<=2),
 ready boolean NOT NULL DEFAULT FALSE,
 writable boolean NOT NULL DEFAULT TRUE,
 acceptance_report jsonb,
 active boolean NOT NULL DEFAULT FALSE,
 created_at timestamptz NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS buyer_search_one_active ON buyer_search_generations((active)) WHERE active;
