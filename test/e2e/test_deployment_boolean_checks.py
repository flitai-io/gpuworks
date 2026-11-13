"""Failure-oriented tests for complete tri-state wire and schema oracles.

Run: PYTHONPATH=test/e2e python3 -B -m unittest test_deployment_boolean_checks
"""
import copy
import json
from pathlib import Path
import unittest

from deployment_boolean_contracts import (
    TRISTATE, assert_wire, evaluate_body, position_body, reviewed_profile,
)


class BooleanContracts(unittest.TestCase):
    def setUp(self):
        path = Path(__file__).resolve().parents[2] / 'ui/tests/fixtures/engines.json'
        self.profiles = {p['name']: p for p in json.loads(path.read_text()) if p['name'] in TRISTATE}
        self.body = {'name': 'owned', 'served_name': 'owned-route', 'model_id': 7,
            'node_uuid': 'owned-node', 'placement_mode': 'auto', 'placement': {'gpus': [0], 'tp': 1},
            'engine': 'vllm', 'quick_params': {'dtype': 'auto', 'enable_cache_report': True},
            'container_overrides': {'env': {'OWNED': 'value'}}}

    def test_both_reviewed_complete_schema_snapshots_and_new_boolean_drift(self):
        for engine, original in self.profiles.items():
            with self.subTest(engine=engine):
                reviewed_profile(original)
                changed = copy.deepcopy(original)
                changed['param_specs'].append({'key': 'future_bool', 'type': 'boolean', 'default': False})
                with self.assertRaisesRegex(AssertionError, 'complete dictionary universe'):
                    reviewed_profile(changed)
                changed = copy.deepcopy(original)
                changed['param_specs'] = [s for s in changed['param_specs'] if s['key'] != 'enable_prefix_caching']
                with self.assertRaisesRegex(AssertionError, 'complete dictionary universe'):
                    reviewed_profile(changed)

    def test_quick_surface_is_reviewed_separately_without_deleting_global_fields(self):
        for change in ('add-chunked', 'remove-prefix'):
            changed = copy.deepcopy(self.profiles['vllm'])
            if change == 'add-chunked': changed['quick_form_keys'].append('enable_chunked_prefill')
            else: changed['quick_form_keys'].remove('enable_prefix_caching')
            with self.subTest(change=change), self.assertRaisesRegex(AssertionError, 'Quick whitelist'):
                reviewed_profile(changed)

    def test_schema_rejects_boolean_coercions_and_unreviewed_accessible_labels(self):
        for value in (0, 1, 'true', None):
            changed = copy.deepcopy(self.profiles['vllm'])
            next(s for s in changed['param_specs'] if s['key'] == 'enable_cache_report')['default'] = value
            with self.subTest(value=value), self.assertRaises(AssertionError):
                reviewed_profile(changed)
        changed = copy.deepcopy(self.profiles['sglang'])
        next(s for s in changed['param_specs'] if s['key'] == 'enable_metrics')['label']['en'] = 'Unreviewed'
        with self.assertRaisesRegex(AssertionError, 'reviewed schema enable_metrics'):
            reviewed_profile(changed)

    def test_on_off_auto_are_strict_and_do_not_weaken_full_wire(self):
        key = 'enable_prefix_caching'
        for position in ('on', 'off', 'auto'):
            expected = position_body(self.body, key, position, 'probe')
            assert_wire(copy.deepcopy(expected), expected, key, position, 'test')
            wrong_values = (None, 0, 1, 'true', 'false') if position != 'auto' else (None, False, True)
            for wrong in wrong_values:
                actual = copy.deepcopy(expected)
                # Auto carries no quick_params at all, so a wrong value for it is
                # an unexpected key on an object that should not exist.
                actual.setdefault('quick_params', {})[key] = wrong
                with self.subTest(position=position, wrong=wrong), self.assertRaises(AssertionError):
                    assert_wire(actual, expected, key, position, 'test')
            actual = copy.deepcopy(expected)
            actual['quick_params_unset'] = [key]
            with self.assertRaisesRegex(AssertionError, 'complete evaluate wire'):
                assert_wire(actual, expected, key, position, 'test')
            actual = copy.deepcopy(expected)
            actual['container_overrides']['env']['OWNED'] = 'wrong'
            with self.assertRaisesRegex(AssertionError, 'complete evaluate wire'):
                assert_wire(actual, expected, key, position, 'test')

    def test_auto_omission_and_false_remain_distinct_and_fixture_is_immutable(self):
        saved = copy.deepcopy(self.body)
        on = position_body(self.body, 'enable_prefix_caching', 'on', 'on')
        off = position_body(self.body, 'enable_prefix_caching', 'off', 'off')
        auto = position_body(self.body, 'enable_prefix_caching', 'auto', 'auto')
        self.assertIs(on['quick_params']['enable_prefix_caching'], True)
        self.assertIs(off['quick_params']['enable_prefix_caching'], False)
        self.assertNotIn('quick_params', auto)
        self.assertEqual(self.body, saved)
        self.assertNotIn('name', auto)
        self.assertNotIn('node_uuid', auto)
        self.assertNotIn('placement', auto)
        manual = {**self.body, 'placement_mode': 'manual'}
        self.assertEqual(evaluate_body(manual)['node_uuid'], 'owned-node')
        self.assertEqual(evaluate_body(manual)['placement'], {'gpus': [0], 'tp': 1})

    def test_a_true_as_false_fault_or_other_field_loss_is_rejected(self):
        key = 'enable_prefix_caching'
        expected = position_body(self.body, key, 'on', 'probe')
        for kind in ('omit', 'flip', 'lose-other-param', 'lose-source'):
            actual = copy.deepcopy(expected)
            if kind == 'omit': actual['quick_params'].pop(key)
            elif kind == 'flip': actual['quick_params'][key] = False
            elif kind == 'lose-other-param': actual.pop('engine')
            else: actual.pop('model_id')
            with self.subTest(kind=kind), self.assertRaises(AssertionError):
                assert_wire(actual, expected, key, 'on', 'test')


if __name__ == '__main__':
    unittest.main()
