import { reportElement as el } from "./reportLanguage";

// Keep the preview's row order and formatting. Switching columns reveals the
// original unrounded metrics on the same rows, including offline-only points.
export function addFullPointColumns(preview: HTMLElement | null, full: HTMLElement, label: string) {
	const head = preview?.querySelector<HTMLTableRowElement>(".ant-table-thead tr");
	const body = preview?.querySelector<HTMLTableSectionElement>(".ant-table-tbody");
	const original = full.querySelector("table");
	if (!preview || !head || !body || !original?.tHead || !original.tBodies[0]) return false;
	preview.id = "all-points";
	const existing = new Map(Array.from(body.querySelectorAll<HTMLTableRowElement>("tr[data-row-key]"), row => [row.dataset.rowKey, row]));
	const addCells = (row: HTMLTableRowElement, source: HTMLTableRowElement) => {
		for (const cell of Array.from(source.cells)) {
			cell.classList.add("full-metric");
			row.append(cell);
		}
	};
	addCells(head, original.tHead.rows[0]);
	const headerTable = head.closest("table");
	const bodyTable = body.closest("table");
	if (bodyTable && headerTable !== bodyTable) {
		// Ant Design splits a scrolling table into a fixed header and a body.
		// The full-width columns need a header in the same horizontal scroller.
		headerTable?.closest(".ant-table-header")?.classList.add("offline-preview-heading");
		const fullHead = bodyTable.createTHead();
		fullHead.className = "offline-full-heading";
		const row = fullHead.insertRow();
		for (const th of head.querySelectorAll(".full-metric")) row.append(th.cloneNode(true));
	}
	for (const source of Array.from(original.tBodies[0].rows)) {
		const seq = source.dataset.seq;
		const match = existing.get(seq);
		const row = match ?? body.insertRow();
		row.dataset.seq = seq;
		if (match) {
			existing.delete(seq);
			const pointCell = Array.from(row.cells).find(cell => cell.textContent?.trim() === `#${seq}`);
			if (pointCell) {
				const anchor = el("a", pointCell.textContent);
				anchor.href = `#point-${seq}`;
				anchor.className = "offline-point-link";
				pointCell.replaceChildren(anchor);
			}
		}
		else row.className = "full-only";
		addCells(row, source);
	}
	for (const row of existing.values()) row.classList.add("preview-only");
	if (original.tBodies[0].rows.length) {
		const toggle = el("input");
		toggle.type = "checkbox";
		toggle.id = "full-point-columns";
		const caption = el("label", label);
		caption.htmlFor = toggle.id;
		const tools = el("div");
		tools.className = "offline-table-controls";
		tools.append(toggle, caption);
		preview.prepend(tools);
	}
	return true;
}
