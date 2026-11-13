"""Inventory drift and independent probe cleanup failure regressions.

Run: PYTHONPATH=test/e2e python3 -B -m unittest test_deployment_boolean_inventory
"""
import copy
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock

from deployment_boolean_inventory_contracts import FIXTURE_GPU, FIXTURE_PARTITION, complete, expected_ids, fixture_hardware, health_contract, health_source, initial
from deployment_boolean_inventory_probe import close_probe
from deployment_boolean_inventory import _enabled, _reason


class InventoryContracts(unittest.TestCase):
    def test_complete_matrix_rejects_missing_duplicate_or_unexplained_absence(self):
        for engine in ('vllm', 'sglang'):
            records = [{'id': name, 'offered': True} for name in expected_ids(engine)]
            complete(records, engine)
            for changed in (records[:-1], records + [records[0]],
                            [{**record, 'offered': False} for record in records]):
                with self.subTest(engine=engine), self.assertRaises(AssertionError):
                    complete(changed, engine)

    def test_fixture_uses_public_gpu_partition_projection_not_absent_hello_caps(self):
        node = {'uuid': 'owned-node', 'gpu_count': 1, 'gpu_vendor': 'nvidia', 'runtime_driver': 'docker',
                'gpus': [{**FIXTURE_GPU, 'partition_capabilities': [FIXTURE_PARTITION]}]}
        fixture_hardware(node, 'owned-node')
        for kind in ('quota-status', 'wrong-memory', 'wrong-runtime', 'wrong-node'):
            changed = copy.deepcopy(node)
            if kind == 'quota-status': changed['gpus'][0]['partition_capabilities'][0]['status'] = 'unavailable'
            elif kind == 'wrong-memory': changed['gpus'][0]['memory_total_bytes'] = 24576
            elif kind == 'wrong-runtime': changed['runtime_driver'] = 'udocker'
            else: changed['uuid'] = 'another-node'
            with self.subTest(kind=kind), self.assertRaises(AssertionError):
                fixture_hardware(changed, 'owned-node')

    def test_health_is_fixed_by_registration_and_effective_template_not_raw_ui(self):
        for local in (True, False):
            body = {'model_id': 1, 'template_id': 2, 'template_spec_name': 'owned',
                    'template_spec_source': 'user', 'model_source': {'provider': 'huggingface'},
                    'container_overrides': {'env': {'E2E_WIZARD' if local else 'E2E_ACQUISITION': 'owned'}}}
            expected = health_source(body)
            self.assertEqual(expected['expected_health'], local)
            self.assertEqual(expected['effective_operations'], ['openai.chat'] if local else [])
            model = {'id': 1, 'source': 'local' if local else 'huggingface',
                     'operations': [{'type': 'openai.chat'}] if local else []}
            template = {'id': 2, 'specs': [{'name': 'owned', 'source': 'user', 'display_name_en': 'Acquisition · BF16', 'display_name_zh_cn': '采集 · BF16',
                         'api': {'health_endpoint': '/health', 'operations': []}}]}
            self.assertEqual(health_contract(body, model, template), expected)
            changed = copy.deepcopy(template)
            changed['specs'][0]['api']['operations'] = [{'type': 'openai.chat'}]
            with self.assertRaisesRegex(AssertionError, 'effective undeclared template'):
                health_contract(body, model, changed)
            changed_model = copy.deepcopy(model)
            changed_model['operations'] = [] if local else [{'type': 'openai.chat'}]
            with self.assertRaisesRegex(AssertionError, 'raw registered model'):
                health_contract(body, changed_model, template)

    def test_initial_state_distinguishes_wire_omission_effective_default_and_auto(self):
        body = {'engine': 'vllm', 'quick_params': {'enable_cache_report': True}}
        self.assertEqual(initial(body, 'enable_cache_report'),
                         {'position': 'on', 'wire': 'explicit', 'value': True})
        self.assertEqual(initial(body, 'enforce_eager'),
                         {'position': 'off', 'wire': 'omitted', 'value': False})
        for key in ('enable_prefix_caching', 'enable_request_metrics'):
            self.assertEqual(initial(body, key), {'position': 'auto', 'wire': 'omitted', 'value': None})
        for wrong in (0, 1, '', 'false', None):
            changed = copy.deepcopy(body)
            changed['quick_params']['enable_cache_report'] = wrong
            with self.subTest(wrong=wrong), self.assertRaises(AssertionError):
                initial(changed, 'enable_cache_report')

    def test_unsupported_enabled_switch_is_a_gap_not_a_silent_contract(self):
        control = SimpleNamespace(is_disabled=lambda: False)
        record, evidence = {'id': 'expert/resource/sleepable', 'disabled_reason': 'Unsupported'}, {'gaps': []}
        _enabled(control, True, record, evidence)
        self.assertEqual(record['actual_disabled'], False)
        self.assertEqual(evidence['gaps'], [{'field': 'expert/resource/sleepable',
            'expected_disabled': True, 'actual_disabled': False, 'reason': 'Unsupported'}])

    def test_generic_help_cannot_substitute_for_the_missing_disabled_reason(self):
        scope = SimpleNamespace(inner_text=lambda: 'Shared disk. Transfers through CPU memory.')
        record, evidence = {'id': 'dictionary/expert/kv_disk_offload'}, {'gaps': []}
        _reason(scope, 'Enable the memory tier first', record, evidence)
        self.assertEqual(len(evidence['gaps']), 1)
        self.assertEqual(evidence['gaps'][0]['missing_disabled_reason'], 'Enable the memory tier first')


class ConditionalPageCleanup(unittest.TestCase):
    def test_normal_close_is_observed_and_exact_listener_is_removed(self):
        page, context, listener, record = MagicMock(), MagicMock(), object(), {}
        page.is_closed.return_value = True
        close_probe(page, context, listener, record)
        page.close.assert_called_once_with()
        context.remove_listener.assert_called_once_with('request', listener)
        self.assertEqual(record, {'page_created': True, 'page_closed': True, 'cleanup_errors': []})

    def test_page_creation_failure_does_not_claim_observed_close(self):
        context, listener, record = MagicMock(), object(), {}
        close_probe(None, context, listener, record, AssertionError('new page failed'))
        self.assertEqual(record, {'page_created': False, 'page_closed': None, 'cleanup_errors': []})
        context.remove_listener.assert_called_once_with('request', listener)

    def test_close_failure_does_not_skip_listener_removal_or_turn_green(self):
        page, context, listener, record = MagicMock(), MagicMock(), object(), {}
        page.close.side_effect = RuntimeError('held page')
        with self.assertRaisesRegex(AssertionError, 'held page'):
            close_probe(page, context, listener, record)
        context.remove_listener.assert_called_once_with('request', listener)
        self.assertEqual(record['page_closed'], False)

    def test_not_really_closed_and_listener_failure_are_both_reported(self):
        page, context, record = MagicMock(), MagicMock(), {}
        page.is_closed.return_value = False
        context.remove_listener.side_effect = RuntimeError('listener retained')
        with self.assertRaisesRegex(AssertionError, 'listener retained'):
            close_probe(page, context, object(), record)
        self.assertEqual(len(record['cleanup_errors']), 2)
        self.assertEqual(record['page_closed'], False)

    def test_primary_field_failure_is_preserved_when_secondary_close_fails(self):
        page, context, record = MagicMock(), MagicMock(), {}
        page.close.side_effect = RuntimeError('close failed')
        close_probe(page, context, object(), record, AssertionError('readonly state wrong'))
        self.assertEqual(record['primary_failure_preserved'], 'readonly state wrong')
        self.assertEqual(record['page_closed'], False)
        self.assertIn('close failed', record['cleanup_errors'][0])


if __name__ == '__main__':
    unittest.main()
