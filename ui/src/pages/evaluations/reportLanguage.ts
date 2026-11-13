import glossary from "./reportZh.json";

const chinese: Record<string, string> = glossary;
const specifier = /%(?:\.\d+)?[sdf]/g;
const escapePattern = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const patterns = Object.entries(chinese).filter(([source]) => /%(?:\.\d+)?[sdf]/.test(source)).map(([source, translation]) => {
	const fields = source.match(specifier)!;
	let regex = escapePattern(source.split(specifier)[0]);
	source.split(specifier).slice(1).forEach((part, i) => {
		regex += (fields[i] === "%s" ? "(.+?)" : "(-?\\d+(?:\\.\\d+)?)") + escapePattern(part);
	});
	return { source, regex: new RegExp("^" + regex + "$"), translation };
});
const definitions = new Map(Object.entries(chinese).filter(([key]) => key.includes("`")).map(([key, val]) => [key.replace(/`/g, ""), val]));

export function reportText(text: string, zh: boolean): string {
	if (!zh) return text;
	if (Object.prototype.hasOwnProperty.call(chinese, text)) return chinese[text];
	const round = /^turn_(\d+)$/.exec(text);
	if (round) return chinese["Turn %s"].replace("%s", round[1]);
	for (const { source, regex, translation } of patterns) {
		const parts = regex.exec(text);
		if (!parts) continue;
		let index = 1;
		return translation.replace(specifier, () => {
			const field = index++;
			const value = parts[field];
			if (source === "%s benchmark for %s" && field === 2) return value;
			if (source.startsWith("best measured point in this run:") && field === 1) return reportText(value, true);
			if (/^turn_\d+$/.test(value)) return reportText(value, true);
			return Object.prototype.hasOwnProperty.call(chinese, value) ? chinese[value] : value;
		});
	}
	return text;
}

export function reportElement<K extends keyof HTMLElementTagNameMap>(tag: K, text?: string | null) {
	const node = document.createElement(tag);
	if (text != null) node.textContent = text;
	return node;
}

const verbatim = new Set(["Model", "Served name", "Model source ID", "Instance UUID", "Node", "Deployment", "Endpoint", "Engine", "Image", "Entrypoint", "argv", "Docker args", "Workload fingerprint", "Failure diagnostic", "Error text", "Raw service usage", "Session / turn / planned turns / attempt"]);

export function localizeLegacy(doc: Document, zh: boolean) {
	if (!zh) return;
	for (const row of doc.querySelectorAll("tr")) {
		const cells = Array.from(row.cells);
		const key = cells[0]?.textContent || "";
		for (const c of cells) {
			const text = c.textContent || "";
			if (!text || c.childElementCount) continue;
			const value = c.tagName === "TH" || (cells.length === 2 && !verbatim.has(key)) ||
				(row.closest("table")?.tHead?.rows[0]?.cells[c.cellIndex]?.textContent === chinese["State"]);
			if (!value) continue;
			const translated = reportText(text, true);
			if (translated !== text) {
				c.title = text;
				c.textContent = translated;
			}
		}
	}
	for (const el of doc.querySelectorAll("h2,h3,h4,summary,li,p,blockquote,svg text,svg title")) {
		const text = el.textContent || "";
		let translated = definitions.get(text) ?? reportText(text, true);
		if (el.matches("blockquote")) {
			const [intro, ...rest] = text.replace(/\.$/, "").split("; ");
			const best = rest.findIndex(part => part.startsWith("best measured point"));
			const sections = best < 0 ? rest : [...rest.slice(0, best), rest.slice(best).join("; ")];
			translated = [intro, ...sections].map(s => reportText(s, true)).join("。") + "。";
		} else if (el.matches("p") && el.querySelector("strong") && text.startsWith("Request detail: ")) {
			const fields = /^Request detail: (.+?); retained records=(\d+)\.\s*(.*)$/.exec(text);
			if (fields) translated = chinese["Request detail: <strong>%s</strong>; retained records=%d. %s"].replace(/<\/?strong>/g, "").replace("%s", () => reportText(fields[1], true)).replace("%d", fields[2]).replace(/%s$/, () => reportText(fields[3], true));
		}
		if (translated === text || el.querySelector("a,table")) continue;
		el.replaceChildren(...translated.split(/(`[^`]+`)/).filter(Boolean).map(part =>
			part.startsWith("`") && part.endsWith("`") ? reportElement("code", part.slice(1, -1)) : part));
	}
	for (const el of doc.querySelectorAll("[aria-label]")) {
		el.setAttribute("aria-label", reportText(el.getAttribute("aria-label")!, true));
	}
}
