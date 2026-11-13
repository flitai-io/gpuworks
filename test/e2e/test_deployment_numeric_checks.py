"""Negative selftests for the independent numeric schema and complete sets.

Run: PYTHONPATH=test/e2e python3 -B -m unittest test_deployment_numeric_checks -v
These are offline helper checks; product mutations and browser acceptance are
separate evidence and cannot be replaced by these injected response objects.
"""
import copy
import unittest
from unittest.mock import MagicMock, patch

import deployment_numeric_checks as numeric
from deployment_launch_checks import same


def api_profile(engine):
    specs = []
    for row in numeric.rows_for(engine).values():
        spec = {name: row[name] for name in numeric.METADATA if row[name] is not None}
        if engine == 'sglang' and row['key'] == 'gpu_memory_utilization':
            spec['default'] = 0.88
        for name in ('label', 'placeholder', 'help'):
            text = {language: value for language, value in row[name].items() if value is not None}
            if text:
                spec[name] = text
        specs.append(spec)
    return {'name': engine, 'param_specs': specs,
            'pd_modes': [{'name': numeric.PD_MODES[engine], 'vendors': ['nvidia']}],
            'quick_form_keys': sorted(set(numeric.rows_for(engine)) & numeric.QUICK_KEYS)}


class NumericInventory(unittest.TestCase):
    def run_dictionary(self, profile):
        page = MagicMock(url='http://127.0.0.1:8802/deploy')
        page.request.get.return_value.status = 200
        page.request.get.return_value.json.return_value = {'items': [profile]}
        report = {}
        rows = numeric.dictionary(page, profile['name'], same, report)
        return rows, report

    def test_both_reviewed_engines_and_unrendered_group(self):
        for engine, total in (('vllm', 17), ('sglang', 8)):
            with self.subTest(engine=engine):
                rows, report = self.run_dictionary(api_profile(engine))
                self.assertEqual(len(rows), total)
                self.assertEqual(rows['data_parallel_size']['group'], 'parallel')
                self.assertEqual(report['numeric_dictionary_snapshot']['unrendered'], numeric.ABSENT)
        with self.assertRaisesRegex(AssertionError, 'no reviewed dictionary'):
            numeric.rows_for('callable')

    def test_missing_extra_or_duplicated_identity_drift_cannot_shrink_universe(self):
        for change in ('missing', 'new', 'renamed', 'duplicate'):
            profile = api_profile('vllm')
            if change == 'missing':
                profile['param_specs'].pop()
            elif change == 'new':
                profile['param_specs'].append({'key': 'unexpected_numeric', 'type': 'number'})
            elif change == 'renamed':
                profile['param_specs'][0]['key'] = 'renamed_numeric'
            else:
                profile['param_specs'].append(copy.deepcopy(profile['param_specs'][0]))
            with self.subTest(change=change), self.assertRaisesRegex(AssertionError, 'complete reviewed numeric dictionary keys'):
                self.run_dictionary(profile)

    def test_bounds_step_type_default_and_unbounded_drift_fail(self):
        for key, name, value in [('gpu_memory_utilization', 'max', 0.99),
                ('gpu_memory_utilization', 'step', 0.02), ('max_model_len', 'max', 4096),
                ('max_num_seqs', 'type', 'number'), ('cpu_offload_gb', 'min', False),
                ('kv_offload_read_threads', 'default', '16')]:
            profile = api_profile('vllm')
            next(row for row in profile['param_specs'] if row['key'] == key)[name] = value
            with self.subTest(key=key, name=name), self.assertRaisesRegex(AssertionError, 'numeric-inventory-schema-' + key):
                self.run_dictionary(profile)

    def test_localized_help_and_quick_whitelist_are_complete(self):
        profile = api_profile('sglang')
        next(row for row in profile['param_specs'] if row['key'] == 'max_model_len')['help'].pop('zh')
        with self.assertRaisesRegex(AssertionError, 'numeric dictionary localized max_model_len help'):
            self.run_dictionary(profile)
        profile = api_profile('sglang')
        profile['quick_form_keys'].remove('max_num_seqs')
        with self.assertRaisesRegex(AssertionError, 'complete numeric quick whitelist'):
            self.run_dictionary(profile)

    def test_real_visible_inventory_rejects_extra_missing_and_duplicate_inputs(self):
        rows = numeric.rows_for('vllm')
        page = MagicMock()
        page.get_by_role.return_value.filter.return_value.evaluate_all.return_value = []
        for actual in ([], ['replicas', 'unexpected'], ['replicas', 'replicas']):
            page.locator.return_value.evaluate_all.return_value = actual
            report = {}
            with self.subTest(actual=actual), self.assertRaisesRegex(AssertionError, 'numeric-inventory-complete-set-selftest'):
                numeric.complete_set(page, {'replicas'}, set(), rows, 'en', same, report, 'selftest')
            self.assertNotIn('numeric_inventory_surfaces', report)

    def test_slider_labels_cannot_disappear_behind_input_inventory(self):
        page = MagicMock()
        page.locator.return_value.evaluate_all.return_value = ['replicas']
        page.get_by_role.return_value.filter.return_value.evaluate_all.return_value = []
        with self.assertRaisesRegex(AssertionError, 'complete slider rows'):
            numeric.complete_set(page, {'replicas'}, {'gpu_memory_utilization'},
                numeric.rows_for('vllm'), 'en', same, {}, 'selftest')

    def test_probe_close_failure_keeps_all_listener_cleanup_stages(self):
        page, context = MagicMock(), MagicMock()
        page.close.side_effect = RuntimeError('probe close failed')
        context.remove_listener.side_effect = RuntimeError('context callback remove failed')
        report = {'numeric_probe_page': {'closed': False}}
        numeric.close_probe(page, context, 'request-fn', 'response-fn', 'console-fn', 'error-fn', report, True)
        self.assertFalse(report['numeric_probe_page']['closed'])
        self.assertEqual([r['stage'] for r in report['numeric_probe_page']['cleanup_errors']],
                         ['close-page', 'remove-listener-request'])
        self.assertEqual([call.args for call in page.remove_listener.call_args_list],
                         [('response', 'response-fn'), ('console', 'console-fn'), ('pageerror', 'error-fn')])

    def test_probe_close_failure_without_primary_cannot_pass(self):
        page = MagicMock()
        page.close.side_effect = RuntimeError('close failed')
        with self.assertRaisesRegex(AssertionError, 'numeric probe page cleanup failed'):
            numeric.close_probe(page, MagicMock(), None, None, None, None,
                                {'numeric_probe_page': {'closed': False}}, False)

    def test_fold_action_checks_real_expanded_owner_and_content(self):
        page, header = MagicMock(), MagicMock()
        page.locator.return_value.filter.return_value = header
        header.get_attribute.return_value = 'false'
        with patch.object(numeric, 'expect') as expect:
            numeric.collapse(page, '高级性能调优')
        page.locator.assert_called_with('.ant-collapse-header')
        page.get_by_text.assert_called_with('高级性能调优', exact=True)
        header.click.assert_called_once()
        self.assertTrue(any(call.args == ('aria-expanded', 'true')
                            for call in expect.return_value.to_have_attribute.call_args_list))
        header.locator.assert_called_with('..')
        header.locator.return_value.locator.assert_called_with('.ant-collapse-content')

    def test_probe_fixture_identity_uses_spec_image_not_parent_override(self):
        fixture = {'model_id': 17, 'template_id': 41, 'template_spec_source': 'user',
                   'template_spec_name': 'browser-vllm', 'engine': 'vllm',
                   'target_vendor': 'nvidia', 'scenario': 'balanced',
                   'image': 'run-owned-override-image'}
        request = {**fixture, 'image': 'gpuworks/fake-engine:test'}
        numeric.fixture_identity(request, fixture, same, 'identity')
        with self.assertRaisesRegex(AssertionError, 'identity'):
            numeric.fixture_identity({**request, 'model_id': 18}, fixture, same, 'identity')
        with self.assertRaisesRegex(AssertionError, 'identity'):
            numeric.fixture_identity(fixture, fixture, same, 'identity')

    def test_all_four_pd_numbers_have_independent_defaults_and_no_invented_max(self):
        self.assertEqual(set(numeric.PD_NUMBERS), {'pd_roles_decode_tp', 'pd_roles_decode_pp',
                                                'pd_roles_prefill_tp', 'pd_roles_prefill_pp'})
        for row in numeric.PD_NUMBERS.values():
            self.assertEqual(row['min'], 1)
            self.assertIsNone(row['max'])
            self.assertEqual(row['step'], 1)
            if row['dimension'] == 'tp':
                self.assertIs(type(row['value']), int)
                self.assertEqual(row['value'], 1)
            else:
                self.assertIsNone(row['value'])
        self.assertEqual(numeric.expected_pd('vllm'), {
            'mode': 'vllm-nixl', 'fallback': 'decode', 'kv_load_failure': 'fail',
            'roles': {'decode': {'tp': 1, 'units': [{}]}, 'prefill': {'tp': 1, 'units': [{}]}}})
        self.assertEqual(numeric.expected_pd('sglang'), {
            'mode': 'sglang-nixl', 'fallback': 'fail',
            'roles': {'decode': {'tp': 1, 'units': [{}]}, 'prefill': {'tp': 1, 'units': [{}]}}})

    def test_three_known_negative_drafts_have_distinct_exact_errors(self):
        # A multi-node draft in automatic placement reports the missing tp first; the
        # node-count error belongs to the manual path, and both are asserted here.
        self.assertEqual(numeric.expected_errors({'nnodes': 2}), [
            {'field': 'placement_mode', 'code': 'AUTO_PLACEMENT_TP_REQUIRED'}])
        self.assertEqual(numeric.expected_errors({'nnodes': 2, 'placement_mode': 'auto'}), [
            {'field': 'placement_mode', 'code': 'AUTO_PLACEMENT_TP_REQUIRED'}])
        self.assertEqual(numeric.expected_errors({'nnodes': 2, 'placement_mode': 'manual'}), [
            {'field': 'node_uuids', 'code': 'DISTRIBUTED_NODE_COUNT', 'params': {'actual': 0, 'required': 2}}])
        self.assertEqual(numeric.expected_errors({'quick_params': {'kv_disk_offload': True}}), [
            {'field': 'shared_kv_dir', 'code': 'QUICK_PARAM_PATH_INVALID', 'params': {'key': 'shared_kv_dir'}}])
        self.assertEqual(numeric.expected_errors({'placement': {'pd': numeric.expected_pd('vllm')}}), [
            {'field': 'placement.pd', 'code': 'PD_NODE_REQUIRED', 'params': {'role': 'decode'}}])
        self.assertEqual(numeric.expected_errors({'quick_params': {'kv_disk_offload': True,
                                                                 'shared_kv_dir': '/e2e-kv-cache'}}), [])

    def test_partition_readiness_is_bound_to_fixture_hardware_not_evaluate_echo(self):
        node = {'uuid': 'owned-node', 'gpu_count': 1, 'gpu_vendor': 'nvidia', 'runtime_driver': 'docker',
                'gpus': [{**numeric.FIXTURE_GPU,
                    'partition_capabilities': [numeric.FIXTURE_PARTITION]}]}
        page = MagicMock(url='http://127.0.0.1:8802/deploy')
        page.request.get.return_value.status = 200
        page.request.get.return_value.json.return_value = node
        report = {}
        numeric.fixture_hardware(page, {'node_uuid': 'owned-node'}, same, report)
        self.assertEqual(numeric.FIXTURE_GPU['memory_total_bytes'], 24 * 2**30)
        self.assertEqual(report['numeric_fixture_hardware']['quota_on_gib'], 16)
        for field, wrong in [('memory_total_bytes', 24576), ('name', 'L4'), ('index', 1)]:
            broken = copy.deepcopy(node)
            broken['gpus'][0][field] = wrong
            page.request.get.return_value.json.return_value = broken
            with self.subTest(field=field), self.assertRaisesRegex(AssertionError, 'exact reported GPU'):
                numeric.fixture_hardware(page, {'node_uuid': 'owned-node'}, same, {})

    def test_shm_help_distinguishes_resolved_and_rejected_fixture_drafts(self):
        labels = {'containerOverrides': {
            'shmHelpResolved': 'Resolved {{size}} GiB including the CPU tier',
            'shmHelpDefault': 'Engine base {{size}} GiB plus the CPU tier',
        }}
        for surface in ('quick-default', 'quick-partition', 'quick-tiers'):
            with self.subTest(surface=surface):
                self.assertEqual(numeric.shm_contract(surface, 1, labels),
                                 ('17', 'Resolved 17 GiB including the CPU tier'))
        for surface in ('quick-multi', 'quick-pd', 'expert-runtime'):
            with self.subTest(surface=surface):
                self.assertEqual(numeric.shm_contract(surface, 1, labels),
                                 ('16', 'Engine base 16 GiB plus the CPU tier'))
        # The actual rejected nnodes=2 fixture has no tier: the two strings
        # still differ even though both branches would display placeholder16.
        self.assertEqual(numeric.shm_contract('quick-multi', 0, labels),
                         ('16', 'Engine base 16 GiB plus the CPU tier'))

    def test_field_and_global_rejection_gates_cannot_both_accept_one_button_state(self):
        class StrictButtonExpectation:
            # Only the Playwright assertion boundary is replaced here; these
            # negative selftests do not replace any management response.
            def __init__(self, enabled):
                self.enabled = enabled
            def to_be_enabled(self):
                if self.enabled is not True:
                    raise AssertionError('expected enabled')
            def to_be_disabled(self):
                if self.enabled is not False:
                    raise AssertionError('expected disabled')
        labels = {'page': {'deploy': 'Deploy'}}
        for scope, expected_enabled in (('field', True), ('global', False)):
            with self.subTest(scope=scope), patch.object(numeric, 'button', return_value=expected_enabled), \
                    patch.object(numeric, 'expect', side_effect=StrictButtonExpectation):
                numeric.rejected_submit_gate(None, labels, scope)
            with self.subTest(scope=scope, wrong_state=True), \
                    patch.object(numeric, 'button', return_value=not expected_enabled), \
                    patch.object(numeric, 'expect', side_effect=StrictButtonExpectation):
                with self.assertRaises(AssertionError):
                    numeric.rejected_submit_gate(None, labels, scope)
        with self.assertRaisesRegex(AssertionError, 'unreviewed numeric rejection scope'):
            numeric.rejected_submit_gate(None, labels, 'unknown')

    def test_required_and_ordinary_fields_cannot_exchange_aria_contracts(self):
        for required, attribute in ((True, 'true'), (False, None)):
            control = MagicMock()
            control.get_attribute.return_value = attribute
            numeric.required_contract(control, required, 'field')
            for wrong in ('false', '', '0', None if required else 'true'):
                control.get_attribute.return_value = wrong
                with self.subTest(required=required, wrong=wrong), self.assertRaisesRegex(
                        AssertionError, 'exact aria-required contract'):
                    numeric.required_contract(control, required, 'field')

    def test_only_required_memory_tier_adds_exact_star_to_dictionary_label(self):
        rows = numeric.rows_for('vllm')
        for tiers in (True, False):
            with patch.object(numeric, 'input_contract') as contract:
                numeric.dictionary_controls(None, {'kv_offloading_size', 'kv_offload_read_threads'},
                    rows, 'vllm', 'en', {'shared': {'automatic': 'Automatic'}}, {}, 'test', tiers=tiers)
            calls = {call.args[1]: call for call in contract.call_args_list}
            memory = calls['kv_offloading_size']
            self.assertEqual(memory.args[2], ('* ' if tiers else '') + 'KV Offload to Memory')
            self.assertIs(memory.kwargs['required'], tiers)
            ordinary = calls['kv_offload_read_threads']
            self.assertEqual(ordinary.args[2], rows['kv_offload_read_threads']['label']['en'])
            self.assertIs(ordinary.kwargs['required'], False)

    def test_distributed_quota_absence_is_wire_context_not_dom_filtered(self):
        record = {'request': {'placement_mode': 'auto', 'nnodes': 2}, 'response': {}}
        numeric.distributed_partition_contract(record, same)
        for wrong in (None, {}, numeric.FIXTURE_PARTITION):
            record['response'] = {'pool_partition_capability': wrong}
            with self.subTest(wrong=wrong), self.assertRaisesRegex(AssertionError, 'must omit'):
                numeric.distributed_partition_contract(record, same)
        for wrong in ({'placement_mode': 'manual', 'nnodes': 2},
                      {'placement_mode': 'auto', 'nnodes': 1}):
            with self.subTest(wrong=wrong), self.assertRaisesRegex(AssertionError, 'fixed distributed quota context'):
                numeric.distributed_partition_contract({'request': wrong, 'response': {}}, same)

    def test_distributed_quota_controls_reject_visible_vram_or_missing_reason(self):
        class ExactExpectation:
            def __init__(self, observed):
                self.observed = observed
            def to_have_count(self, expected):
                if type(self.observed) is not int or self.observed != expected:
                    raise AssertionError('unexpected vram count')
            def to_be_visible(self):
                if self.observed is not True:
                    raise AssertionError('missing exact reason')
        for count, visible in ((0, True), (1, True), (0, False)):
            page = MagicMock()
            page.locator.return_value.filter.return_value = count
            page.get_by_text.return_value = visible
            labels = {'resource': {'dgpuUnavailablePool': 'This pool cannot provide quota'}}
            with patch.object(numeric, 'expect', side_effect=ExactExpectation):
                if count == 0 and visible:
                    numeric.distributed_quota_controls(page, labels)
                else:
                    with self.assertRaises(AssertionError):
                        numeric.distributed_quota_controls(page, labels)
            if count == 0:
                page.get_by_text.assert_called_with('This pool cannot provide quota', exact=True)

    def test_health_threshold_disabled_uses_existing_registration_oracle(self):
        for acquisition in (True, False):
            body = {'container_overrides': {'env': {
                'E2E_ACQUISITION' if acquisition else 'E2E_WIZARD': 'owned'}}}
            report = {'numeric_health_source': numeric.health_source(body)}
            labels = {'containerOverrides': {'shmHelpDefault': 'base {{size}}'},
                      'catalog': {'replicas': 'Replicas'}}
            common = {'healthCheck': {'threshold': 'Threshold', 'seconds': 'seconds'}}
            # Other static field definitions need their reviewed labels too.
            labels.update({'workbench': {'resource': {'tp': 'TP', 'pp': 'PP'}, 'automatic': 'Automatic',
                                        'node': {'nodeCount': 'Nodes'}},
                           'resource': {'tensorParallel': 'TP', 'pipelineParallel': 'PP',
                                        'nodeCount': 'Nodes', 'masterPort': 'Port',
                                        'partition': 'Quota', 'fullGpu': 'Full'},
                           'containerOverrides': {'shmHelpDefault': 'base {{size}}', 'shm': 'SHM',
                                                  'startTimeout': 'Start', 'seconds': 'seconds'}})
            with patch.object(numeric, 'input_contract') as contract:
                numeric.static_controls(None, numeric.CORE, labels, common, report, 'expert-model')
            self.assertIs(contract.call_args.kwargs['disabled'], acquisition)
            self.assertIsNone(contract.call_args.args[3])
            self.assertEqual(contract.call_args.args[7], '60')

    def test_pd_omission_rejects_explicit_null_and_any_other_value(self):
        numeric.pd_omission_contract({'nnodes': 1})
        for key in ('placement_mode', 'node_uuid', 'vram_limit_gib'):
            for value in (None, '', 0, 'manual'):
                with self.subTest(key=key, value=value), self.assertRaisesRegex(AssertionError, 'must omit ' + key):
                    numeric.pd_omission_contract({'nnodes': 1, key: value})

    def test_literal_surface_table_covers_both_profiles_without_pd_shrinking(self):
        base = {'max_model_len', 'max_num_batched_tokens', 'max_num_seqs'}
        roles = {'pd_roles_decode_tp', 'pd_roles_decode_pp', 'pd_roles_prefill_tp', 'pd_roles_prefill_pp'}
        for engine in ('vllm', 'sglang'):
            rows = numeric.rows_for(engine)
            pd = numeric.SURFACES[engine]['quick-pd']
            self.assertEqual(set(pd['numbers']), base | (numeric.TIER_KEYS if engine == 'vllm' else set()))
            self.assertEqual(set(pd['pd_fields']), roles)
            self.assertEqual(pd['fold'], 'quick')
            self.assertEqual(set(pd['static']), numeric.CORE | {'replicas'} | numeric.RUNTIME)
            for surface, contract in numeric.SURFACES[engine].items():
                with self.subTest(engine=engine, surface=surface):
                    numbers, sliders = set(contract['numbers']), set(contract['sliders'])
                    self.assertTrue(numbers | sliders <= set(rows))
                    self.assertFalse(numbers & sliders)
                    self.assertNotIn('data_parallel_size', numbers | sliders)
                    self.assertTrue(all(rows[key]['control'] != 'slider' for key in numbers))
                    self.assertTrue(all(rows[key]['control'] == 'slider' for key in sliders))
                    self.assertEqual(len(numbers), len(contract['numbers']))
                    if surface.startswith('quick'):
                        self.assertEqual(contract['fold'], 'quick')
            self.assertEqual(set(numeric.SURFACES[engine]['expert-performance']['numbers']),
                {key for key, row in rows.items() if row['group'] in
                 ('performance', 'performance_advanced', 'speculative') and row['control'] != 'slider'} - numeric.TIER_KEYS)

    def test_pd_fold_state_and_field_loops_share_the_full_literal_oracle(self):
        numbers = {'max_model_len', 'max_num_batched_tokens', 'max_num_seqs', 'kv_offloading_size',
                   'kv_offload_read_threads', 'kv_offload_write_threads', 'kv_disk_limit_gib'}
        static = {'health_check_threshold_sec', 'replicas', 'container-overrides-shm', 'container-overrides-start-timeout'}
        roles = {'pd_roles_decode_tp', 'pd_roles_decode_pp', 'pd_roles_prefill_tp', 'pd_roles_prefill_pp'}
        rows = numeric.rows_for('vllm')
        labels = {'workbench': {'advanced': {'title': 'Advanced'}},
                  'pd': {'tpAria': '{{role}} TP', 'ppAria': '{{role}} PP',
                         'role': {'decode': 'Decode', 'prefill': 'Prefill'}}}
        # This models only the helper's fold/visibility boundary, not the
        # management API or a replacement for actual product/browser evidence.
        for opens in (True, False):
            state = {'open': False, 'observed': set()}
            page = MagicMock()
            page.locator.return_value.evaluate_all.side_effect = lambda script: sorted(
                numbers | static | roles if state['open'] else
                (numbers | static | roles) - {'max_num_batched_tokens', 'max_num_seqs'})
            page.get_by_role.return_value.filter.return_value.evaluate_all.return_value = [rows['gpu_memory_utilization']['label']['en']]
            def unfold(*args):
                state['open'] = opens
            def dictionary_fields(page, keys, *args, **kwargs):
                self.assertEqual(keys, numbers | {'gpu_memory_utilization'})
                if not state['open']:
                    raise AssertionError('collapsed surface hides required automatic numbers')
                state['observed'].update(keys)
            def static_fields(page, keys, *args, **kwargs):
                self.assertEqual(keys, static)
                state['observed'].update(keys)
            def role_field(page, key, *args, **kwargs):
                self.assertIn(key, roles)
                state['observed'].add(key)
            with patch.object(numeric, 'collapse', side_effect=unfold), \
                    patch.object(numeric, 'dictionary_controls', side_effect=dictionary_fields), \
                    patch.object(numeric, 'static_controls', side_effect=static_fields), \
                    patch.object(numeric, 'input_contract', side_effect=role_field):
                if opens:
                    report = {}
                    numeric.surface_contract(page, 'vllm', 'quick-pd', rows, 'en', labels, {}, same, report)
                    self.assertEqual(state['observed'], numbers | static | roles | {'gpu_memory_utilization'})
                    self.assertEqual(report['numeric_inventory_surfaces'][0]['input_keys'], sorted(numbers | static | roles))
                else:
                    with self.assertRaisesRegex(AssertionError, 'collapsed surface'):
                        numeric.surface_contract(page, 'vllm', 'quick-pd', rows, 'en', labels, {}, same, {})
        with self.assertRaisesRegex(AssertionError, 'unreviewed numeric surface'):
            numeric.surface_contract(None, 'vllm', 'unreviewed', rows, 'en', labels, {}, same, {})

    def test_pd_complete_set_still_rejects_the_two_missing_automatic_inputs(self):
        contract = numeric.SURFACES['vllm']['quick-pd']
        wanted = set(contract['numbers']) | set(contract['static']) | set(contract['pd_fields'])
        page = MagicMock()
        page.locator.return_value.evaluate_all.return_value = sorted(wanted - {'max_num_batched_tokens', 'max_num_seqs'})
        page.get_by_role.return_value.filter.return_value.evaluate_all.return_value = ['GPU Memory Utilization']
        with self.assertRaisesRegex(AssertionError, 'numeric-inventory-complete-set-quick-pd'):
            numeric.complete_set(page, wanted, set(contract['sliders']), numeric.rows_for('vllm'), 'en', same, {}, 'quick-pd')

    def test_every_accepted_evaluate_is_bound_to_fixed_fixture_identity(self):
        expected = {'model_id': 7, 'template_id': 11, 'template_spec_source': 'user',
                    'template_spec_name': 'browser-vllm', 'engine': 'vllm', 'target_vendor': 'nvidia',
                    'scenario': 'balanced', 'image': 'gpuworks/fake-engine:test', 'served_name': 'owned-numeric'}
        report = {'numeric_expected_identity': expected}
        def evaluate(request):
            return lambda *args: {'request': request, 'response': {'ok': True}}
        numeric.checked_evaluation(None, None, None, evaluate(copy.deepcopy(expected)), same, report, 'identity')
        for key in expected:
            changed = {**expected, key: 'different'}
            with self.subTest(key=key), self.assertRaisesRegex(AssertionError, 'fixed fixture identity'):
                numeric.checked_evaluation(None, None, None, evaluate(changed), same, report, 'identity')
        with self.assertRaisesRegex(AssertionError, 'fixed fixture identity'):
            numeric.checked_evaluation(None, None, None, evaluate({**expected, 'model_id': True}), same, report, 'identity')

    def test_unschedulable_projection_has_exact_errors_and_boolean_ok(self):
        error = {'field': 'node_uuids', 'code': 'DISTRIBUTED_NODE_COUNT', 'params': {'actual': 0, 'required': 2}}
        for response in ({'ok': True, 'errors': [error]}, {'ok': False, 'errors': []},
                {'ok': False, 'errors': [{**error, 'params': {'actual': 1, 'required': 2}}]},
                {'ok': 0, 'errors': [error]}):
            def evaluation(*args):
                return {'response': copy.deepcopy(response)}
            with self.subTest(response=response), self.assertRaises(AssertionError):
                numeric.checked_evaluation(None, None, None, evaluation, same, {}, 'no-fit', [error])


if __name__ == '__main__':
    unittest.main()
