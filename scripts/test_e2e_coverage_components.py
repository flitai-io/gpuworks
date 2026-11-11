#!/usr/bin/env python3
"""Offline tests for checklist denominators and product-derived enumeration metrics."""
import tempfile
import unittest
from pathlib import Path

from e2e_coverage_components import STANDARD, components
from e2e_coverage_inventory import inventory


class ComponentsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        standard = "## 1. Form\n" + "\n".join(f"{number}. check {number}" for number in range(1, 10))
        self.write(STANDARD, standard + "\n## 5. Select\n1. options\n2. choose\n3. keyboard\n")

    def write(self, path, text):
        file = self.root / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(text)

    def data(self, details, **extra):
        return {"contracts": [{"id": "contract/example", "manifest": "test/e2e/coverage/example.json",
                                "module": "models", "kind": "business", "details": details, **extra}]}

    def checklist(self, details):
        return [row for row in components(self.root, self.data(details)) if row["dimension"] == "checklist"]

    def test_one_of_nine_is_11_1_percent_and_untouched_items_unknown(self):
        row, = self.checklist({"checklist_items": {"covered_in_this_scenario": ["1.1"]}})
        self.assertEqual((row["numerator"], row["denominator"], row["percent"]), (1, 9, 11.1))
        self.assertEqual(len(row["unknown"]), 8)
        self.assertIn("不是运行通过率", row["basis"])

    def test_partial_missing_and_justified_na(self):
        row, = self.checklist({"checklist_items": {
            "1.1": "PARTIAL: default tested; keyboard MISSING",
            "missing": {"1.2": "No negative case"},
            "not_applicable": {"1.3": "No editable form in this product"},
            "1.4": "exact persisted value"}})
        self.assertEqual((row["numerator"], row["denominator"]), (1, 8))
        self.assertEqual(len(row["not_applicable"]), 1)
        self.assertEqual(len(row["missing"]), 2)
        self.assertEqual(len(row["unknown"]), 5)

    def test_unjustified_na_does_not_remove_denominator(self):
        for bindings in [
            {"not_applicable": {"1.1": ""}}, {"not_applicable": ["1.1"]},
            {"1.1": "N/A"}, [{"item": "1.1", "status": "N/A"}],
            [{"item": "1.1", "status": "N/A", "reason": "N/A"}],
        ]:
            with self.subTest(bindings=bindings):
                row, = self.checklist({"checklist": bindings})
                self.assertEqual(row["denominator"], 9)
                self.assertFalse(row["not_applicable"])
                self.assertEqual(len(row["unknown"]), 9)

    def test_schema_variants_ranges_and_conflicting_declarations(self):
        rows = self.checklist({"checklist": [{"item": "1.1-1.3", "status": "covered", "reason": "precise persisted values"}],
                               "checklist_items": {"partial": {"1.2": "one variant only"}},
                               "dimensions": {"checklist": ["5.1", "5.2"], "partial_checklist": {"5.3": "focus missing"}}})
        form, select = rows
        self.assertEqual((form["numerator"], form["denominator"]), (2, 9))
        self.assertEqual((select["numerator"], select["denominator"]), (2, 3))
        self.assertTrue(any("partial" in value for value in form["missing"]))

    def test_section_summary_and_generic_dimension_tags_do_not_claim_every_item(self):
        row, = self.checklist({"checklist_items": {"1": "covered for happy path only"}, "dimensions": ["5.1"]})
        self.assertEqual(row["numerator"], 0)
        self.assertEqual(row["denominator"], 9)

    def test_fields_do_not_inherit_case_select_checklist_or_count_sentences(self):
        rows = components(self.root, self.data({"checklist_items": {"5.1": "actual options"},
            "fields": [{"field_id": "model.source", "component": "Select", "options": ["a", "b"],
                        "covered": ["a", "keyboard", "persistence"], "missing": ["b"]}]}))
        field = next(row for row in rows if row["dimension"] == "field")
        self.assertIsNone(field["denominator"])
        self.assertIsNone(field["percent"])
        self.assertEqual(field["covered"], ["a", "keyboard", "persistence"])
        self.assertTrue(field["unknown"])

    def test_unknown_schema_preserves_fields_and_excludes_runner_or_conflict(self):
        data = self.data({"fields": {"new-selector": "new unsupported declaration"}, "checklist": "unsupported"})
        rows = components(self.root, data)
        self.assertEqual(len(rows), 1)
        self.assertIsNone(rows[0]["percent"])
        for flags in [{"kind": "runner"}, {"conflict": True}]:
            self.assertEqual(components(self.root, self.data({}, **flags)), [])

    def test_explicit_unknown_status_is_never_covered(self):
        for status in ("UNKNOWN", "pending", "", "REQUIRED"):
            with self.subTest(status=status):
                row, = self.checklist({"checklist": [{"item": "1.1", "status": status, "reason": "Awaiting evidence"}]})
                self.assertEqual(row["numerator"], 0)
                self.assertEqual(len(row["unknown"]), 9)
        for status in ("UNKNOWN", "pending", "NOT_RUN: no report"):
            row, = self.checklist({"checklist_items": {"1.1": status}})
            self.assertEqual(row["numerator"], 0)
            self.assertEqual(len(row["unknown"]), 9)

    def test_components_and_inventory_controls_are_visible_with_source_context(self):
        details = {"fields": [{"field_id": "same-id", "component": "Select", "missing": ["field gap"]}],
                   "components": [{"id": "image.engine-filter", "type": "Menu/Checkbox", "dimensions": ["engine A"]},
                                  {"id": "same-id", "type": "Select", "missing": ["component gap"]}],
                   "inventory": [{"component": "ModelPicker", "fields": ["model_id"], "contract": "Readable name"},
                                 {"component": "Weight confirmation", "fields": ["model_id"], "contract": "Confirm model"}]}
        rows = [row for row in components(self.root, self.data(details)) if row["dimension"] == "field"]
        self.assertEqual(len(rows), 4)
        self.assertEqual(len({row["id"] for row in rows}), 4)
        same = next(row for row in rows if row["id"].endswith("/field-same-id"))
        self.assertEqual(same["missing"], ["field gap", "component gap"])
        self.assertEqual(len([row for row in rows if "model_id" in row["name"]]), 2)
        filter_row = next(row for row in rows if "engine-filter" in row["name"])
        self.assertTrue(any("/components/0/dimensions" in value for value in filter_row["assertions"]))
        self.assertIsNone(filter_row["percent"])

    def test_real_filter_controls_inventory_and_value_bindings(self):
        root = Path(__file__).resolve().parents[1]
        rows = components(root, inventory(root))
        filters = [row for row in rows if row["manifest"].endswith("image-catalog-filters.json")]
        for control in ("image-series.engine-filter", "image-series.framework-filter", "image-series.enabled-filter", "image-series.source"):
            row = next(row for row in filters if row["dimension"] == "field" and control in row["name"])
            self.assertIsNone(row["percent"])
        metric = next(row for row in filters if row["name"] == "image-series.enabled-filter 可选值")
        self.assertEqual((metric["numerator"], metric["denominator"]), (2, 2))
        self.assertEqual(metric["value_contract_ids"], {value: ["gpuworks/image-catalog/series-filter-matrix"] for value in ("enabled", "disabled")})
        architecture = next(row for row in rows if row["name"] == "image.architecture 可选值")
        self.assertEqual(set(architecture["value_contract_ids"]), {"multi", "amd64", "arm64"})
        self.assertTrue(all(ids == ["gpuworks/image-catalog/architecture-options"] for ids in architecture["value_contract_ids"].values()))
        self.assertTrue(any("HealthCheckFields" in row["name"] and row["dimension"] == "field" for row in rows))

    def test_controls_schema_preserves_seven_real_network_controls_and_gaps(self):
        root = Path(__file__).resolve().parents[1]
        rows = [row for row in components(root, inventory(root))
                if row["manifest"].endswith("network-type-roles.json") and row["dimension"] == "field"]
        self.assertEqual({row["id"].split("/field-")[-1] for row in rows},
                         {"name", "description", "type", "roles", "cidr", "interface", "advanced"})
        cidr = next(row for row in rows if row["id"].endswith("/field-cidr"))
        self.assertIn("IPv6", cidr["missing"])
        self.assertTrue(any("independent GET" in value for value in cidr["covered"]))
        self.assertTrue(any("/controls/4/persisted" in value for value in cidr["assertions"]))
        self.assertIsNone(cidr["percent"])

    def test_other_control_summary_and_keyed_controls_preserve_gaps(self):
        data = self.data({"controls": {"search": {"component": "Search", "missing": ["no match"]}},
                          "other_controls": {"inventory": "other-manifest.json", "missing": ["live refresh"]}})
        rows = components(self.root, data)
        self.assertEqual(len(rows), 2)
        self.assertEqual(next(row for row in rows if "search" in row["name"])["missing"], ["no match"])
        summary = next(row for row in rows if "other_controls" in row["name"])
        self.assertEqual(summary["missing"], ["live refresh"])
        self.assertTrue(any("other-manifest.json" in value for value in summary["assertions"]))

    def test_image_actual_option_flow_slices_filters_and_rebinding_degrade_to_unknown(self):
        root = Path(__file__).resolve().parents[1]
        contracts = [row for row in inventory(root)["contracts"] if row["id"] == "gpuworks/image-catalog/architecture-options"]
        file = "test/e2e/image_catalog_enums.py"
        original = (root / file).read_text()
        self.write("ui/src/pages/images/ImageImportDrawer.tsx", (root / "ui/src/pages/images/ImageImportDrawer.tsx").read_text())
        mutations = [
            original.replace("field, options = dimensions[dimension]", "field, options = dimensions[dimension]\n    options = options[:1]"),
            original.replace("field, options = dimensions[dimension]", "field, options = dimensions[dimension]\n    options = [item for item in options if item[0] == 'multi']"),
            original.replace("field, options = dimensions[dimension]", "field, options = dimensions[dimension]\n    options = FRAMEWORKS"),
            original.replace('("multi", "amd64", "arm64")]', '("multi", "amd64", "arm64") if value == "multi"]'),
            original.replace('[(value, value) for value', '[("multi", value) for value'),
            original.replace('enumerate(options):', 'enumerate(options[:1]):'),
            original.replace('arch = value if dimension == "architectures" else "multi"', 'arch = "multi"'),
        ]
        for changed in mutations:
            with self.subTest(changed=changed != original):
                self.assertNotEqual(changed, original)
                self.write(file, changed)
                row = next(row for row in components(self.root, {"contracts": contracts}) if row["dimension"] == "enum")
                self.assertIsNone(row["percent"])
                self.assertFalse(row["covered"])
                self.assertFalse(any(row["value_contract_ids"].values()))

    def test_token_and_network_rebinding_degrade_to_unknown(self):
        root = Path(__file__).resolve().parents[1]
        cases = [
            ("test/e2e/platform_token_options.py", "platform-token-options.json", "scope, days = CASES[case]", "scope, days = CASES[case]\n    days = 0", "ui/src/pages/apikeys/PlatformTokensCard.tsx"),
            ("test/e2e/network_type_roles.py", "network-type-roles.json", "kind, roles = COMBINATIONS[key]", "kind, roles = COMBINATIONS[key]\n    roles = roles[:1]", "internal/server/netplan/vocab.go"),
            ("test/e2e/network_type_roles.py", "network-type-roles.json", "for roles in itertools.combinations(TYPE_ROLES[kind], size)", "for roles in itertools.combinations(TYPE_ROLES[kind], size) if size == 1", "internal/server/netplan/vocab.go"),
        ]
        for file, manifest, old, new, product in cases:
            with self.subTest(file=file, new=new):
                data = {"contracts": [row for row in inventory(root)["contracts"] if row["manifest"].endswith(manifest)]}
                original = (root / file).read_text()
                self.assertIn(old, original)
                self.write(file, original.replace(old, new))
                self.write(product, (root / product).read_text())
                metrics = [row for row in components(self.root, data) if row["dimension"] in {"enum", "combination"}]
                self.assertTrue(metrics)
                self.assertTrue(all(row["percent"] is None or row["numerator"] == 0 for row in metrics))
                self.assertFalse(any(row["covered"] for row in metrics))

    def test_component_ids_are_globally_unique_across_modules_in_shared_manifest(self):
        root = Path(__file__).resolve().parents[1]
        rows = components(root, inventory(root))
        self.assertEqual(len(rows), len({row["id"] for row in rows}))
        shared = [row for row in rows if row["manifest"].endswith("network-storage-lifecycle.json")]
        for suffix in ("checklist-1", "field-name"):
            selected = [row for row in shared if row["id"].endswith("/" + suffix)]
            self.assertEqual({row["module"] for row in selected}, {"networks", "storage"})
            self.assertEqual(len({row["id"] for row in selected}), 2)
            self.assertTrue(all(row["module"] + "/" + suffix in row["id"] for row in selected))

    def test_real_inventory_sections_and_enum_denominators(self):
        root = Path(__file__).resolve().parents[1]
        rows = components(root, inventory(root))
        metrics = {row["name"]: row for row in rows if row["dimension"] in {"enum", "combination"}}
        expected = {"image.architecture 可选值": 3, "image.framework 可选值": 7,
                    "host_ethernet 非空角色组合": 7, "host_rdma 非空角色组合": 7,
                    "accelerator_rdma 非空角色组合": 1, "accelerator_hccs 非空角色组合": 1,
                    "平台令牌 scope × expiry 组合": 8, "平台令牌 scope 可选值": 2, "平台令牌 expiry 可选值": 4}
        for name, count in expected.items():
            with self.subTest(name=name):
                self.assertEqual((metrics[name]["numerator"], metrics[name]["denominator"], metrics[name]["percent"]), (count, count, 100.0))
                self.assertTrue(metrics[name]["assertions"])
        self.assertIsNone(metrics["image.engine 可选值"]["denominator"])
        self.assertIsNone(metrics["image.engine 可选值"]["percent"])
        self.assertEqual(len({row["source"].split("#")[-1] for row in rows if row["dimension"] == "checklist"}), 23)

    def test_removing_token_leaf_reduces_combination_registration(self):
        root = Path(__file__).resolve().parents[1]
        data = inventory(root)
        data["contracts"] = [row for row in data["contracts"] if row["id"] != "gpuworks/platform-token/options/read-0"]
        row = next(row for row in components(root, data) if row["name"] == "平台令牌 scope × expiry 组合")
        self.assertEqual((row["numerator"], row["denominator"], row["percent"]), (7, 8, 87.5))
        self.assertEqual(row["missing"], ["read × 0"])

    def test_network_missing_leaf_retains_full_product_universe(self):
        root = Path(__file__).resolve().parents[1]
        data = inventory(root)
        data["contracts"] = [row for row in data["contracts"] if row["id"] != "gpuworks/network/type-role/host_rdma--compute"]
        row = next(row for row in components(root, data) if row["name"] == "host_rdma 非空角色组合")
        self.assertEqual((row["numerator"], row["denominator"], row["percent"]), (6, 7, 85.7))
        self.assertEqual(row["missing"], ["host_rdma--compute"])

    def test_enabled_filter_new_option_and_missing_assertion_do_not_fake_100(self):
        root = Path(__file__).resolve().parents[1]
        contracts = [row for row in inventory(root)["contracts"] if row["id"] == "gpuworks/image-catalog/series-filter-matrix"]
        file = "test/e2e/image_catalog_filters.py"
        test_source = (root / file).read_text()
        self.write(file, test_source)
        ui_file = "ui/src/pages/images/ImageSeriesTable.tsx"
        ui = (root / ui_file).read_text().replace('filters: [{ value: "enabled",', 'filters: [{ value: "unknown", text: "Unknown" }, { value: "enabled",')
        self.write(ui_file, ui)
        row = next(row for row in components(self.root, {"contracts": contracts}) if row["dimension"] == "enum")
        self.assertEqual((row["numerator"], row["denominator"], row["percent"]), (2, 3, 66.7))
        self.assertEqual(row["missing"], ["unknown"])
        self.assertEqual(row["value_contract_ids"]["unknown"], [])
        self.write(file, test_source.replace('assert_rows(expected, f"{field}/option:{option}")', 'assert_rows(custom, "weak assertion")'))
        row = next(row for row in components(self.root, {"contracts": contracts}) if row["dimension"] == "enum")
        self.assertIsNone(row["percent"])
        self.assertFalse(row["covered"])

    def test_image_enum_subset_is_not_assumed_complete_without_product_source(self):
        data = self.data({"dimensions": {"enum_options": ["multi"]},
                          "fields": [{"field_id": "image.architecture", "component": "Select"}]},
                         test_file="test/e2e/image_catalog_enums.py")
        row = next(row for row in components(self.root, data) if row["dimension"] == "enum")
        self.assertIsNone(row["percent"])
        self.assertIsNone(row["denominator"])

    def test_product_option_added_beyond_tested_set_is_visible_missing(self):
        root = Path(__file__).resolve().parents[1]
        data = inventory(root)
        architecture = [row for row in data["contracts"] if row["id"] == "gpuworks/image-catalog/architecture-options"]
        self.write("test/e2e/image_catalog_enums.py", (root / "test/e2e/image_catalog_enums.py").read_text())
        ui = (root / "ui/src/pages/images/ImageImportDrawer.tsx").read_text()
        self.write("ui/src/pages/images/ImageImportDrawer.tsx", ui.replace('{ value: "arm64", label: "arm64" },', '{ value: "arm64", label: "arm64" }, { value: "riscv64", label: "riscv64" },'))
        row = next(row for row in components(self.root, {"contracts": architecture}) if row["dimension"] == "enum")
        self.assertEqual((row["numerator"], row["denominator"], row["percent"]), (3, 4, 75.0))
        self.assertEqual(row["missing"], ["riscv64"])
        self.assertEqual(row["value_contract_ids"]["riscv64"], [])
        self.assertEqual(row["value_contract_ids"]["arm64"], ["gpuworks/image-catalog/architecture-options"])


if __name__ == "__main__":
    unittest.main()
