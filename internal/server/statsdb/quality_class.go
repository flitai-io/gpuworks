package statsdb

// schemaV4 splits the service quality by request priority class: the hour
// and day rows key on the class the gateway ran the requests at as well, so
// the quality of each class can be read apart (a low class starved behind a
// high one shows). Rows written before carry the empty class. SQLite cannot
// change a primary key, so the tables are rebuilt.
var schemaV4 = []string{
	`ALTER TABLE quality_hourly RENAME TO quality_hourly_v3`,
	`CREATE TABLE quality_hourly (
		hour_ms INTEGER NOT NULL, deployment_id INTEGER NOT NULL, priority_class TEXT NOT NULL DEFAULT '',
		requests INTEGER NOT NULL, completed INTEGER NOT NULL, length INTEGER NOT NULL,
		client_closed INTEGER NOT NULL, interrupted INTEGER NOT NULL, error INTEGER NOT NULL,
		ttft_hist BLOB NOT NULL, e2e_hist BLOB NOT NULL,
		output_tokens INTEGER NOT NULL, decode_ms INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, deployment_id, priority_class))`,
	`INSERT INTO quality_hourly SELECT hour_ms, deployment_id, '', requests, completed, length, client_closed,
		interrupted, error, ttft_hist, e2e_hist, output_tokens, decode_ms FROM quality_hourly_v3`,
	`DROP TABLE quality_hourly_v3`,
	`ALTER TABLE quality_daily RENAME TO quality_daily_v3`,
	`CREATE TABLE quality_daily (
		day INTEGER NOT NULL, deployment_id INTEGER NOT NULL, priority_class TEXT NOT NULL DEFAULT '',
		requests INTEGER NOT NULL, completed INTEGER NOT NULL, length INTEGER NOT NULL,
		client_closed INTEGER NOT NULL, interrupted INTEGER NOT NULL, error INTEGER NOT NULL,
		ttft_hist BLOB NOT NULL, e2e_hist BLOB NOT NULL,
		output_tokens INTEGER NOT NULL, decode_ms INTEGER NOT NULL,
		PRIMARY KEY (day, deployment_id, priority_class))`,
	`INSERT INTO quality_daily SELECT day, deployment_id, '', requests, completed, length, client_closed,
		interrupted, error, ttft_hist, e2e_hist, output_tokens, decode_ms FROM quality_daily_v3`,
	`DROP TABLE quality_daily_v3`,
}
