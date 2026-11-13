export interface SavedReportContext {
	run_id: number;
	dataset?: { profile?: object };
	target_snapshot?: unknown;
	tokenizer?: string;
}

// The legacy CSV carries the server-redacted snapshot in the first result row.
// Quoted fields can themselves contain commas, newlines and escaped quotes.
export function savedReportContext(csv: string, runID: number): SavedReportContext | undefined {
	const rows: string[][] = [];
	let row: string[] = [], field = "", quoted = false;
	for (let i = 0; i <= csv.length && rows.length < 2; i++) {
		const char = csv[i];
		if (char === '"') {
			if (quoted && csv[i + 1] === '"') { field += '"'; i++; }
			else quoted = !quoted;
		} else if (!quoted && (char === "," || char === "\n" || char === undefined)) {
			row.push(field.replace(/\r$/, ""));
			field = "";
			if (char !== ",") { rows.push(row); row = []; }
		} else if (char !== undefined) field += char;
	}
	const raw = rows[1]?.[rows[0]?.indexOf("report_context_json") ?? -1];
	if (!raw) return;
	try {
		const value = JSON.parse(raw);
		if (value?.run_id === runID) return value;
	} catch { /* Older exports may omit the structured context. */ }
}

export async function fetchReportContext(href: string, runID: number) {
	const url = new URL(href, location.href);
	url.searchParams.set("format", "csv");
	try {
		const response = await fetch(url);
		if (response.ok && response.headers.get("content-type")?.startsWith("text/csv")) return savedReportContext(await response.text(), runID);
	} catch { /* The original HTML already includes the saved target and point evidence. */ }
}
