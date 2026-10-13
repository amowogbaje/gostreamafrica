-- Indexes for the catalog read paths added in Phase 1 Step 3.

-- Filter by language, newest first (language filter and the per-language home rows).
CREATE INDEX titles_language_published_idx ON titles (original_language, published_at DESC, id)
    WHERE status = 'published';

-- Filter by type, newest first.
CREATE INDEX titles_type_published_idx ON titles (type, published_at DESC, id)
    WHERE status = 'published';

-- Sort by name with a stable tie-break.
CREATE INDEX titles_name_idx ON titles (name, id);

-- "Trending": distinct viewers in a recent window.
CREATE INDEX watch_progress_recent_idx ON watch_progress (updated_at DESC);
