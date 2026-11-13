"""Reviewed boolean universe for the six standalone fake-engine contracts.

Sources: deploy/param_specs.go; deployParamApplicability.ts; FeatureChoice,
DeployParamField and DeployMetricsField. These constants are an independent
oracle: live schema/DOM never determine which booleans the test expects.
"""
import copy
import json

from platform_token_read_revoke import check

# key, default (None means genuinely unspecified), group, control, quick layout,
# English label, Chinese label. Conditional tier is listed, never called Auto.
COMMON = (
    ('trust_remote_code', False, 'model', 'switch', False, 'Trust Remote Code', '信任远程代码'),
    ('enable_auto_tool_choice', False, 'model', 'switch', False, 'Automatic Tool Selection', '自动选择工具'),
    ('enable_prefix_caching', None, 'performance', 'switch', True, 'Prefix Caching', '前缀缓存'),
    ('enable_cache_report', True, 'performance', 'switch', True, 'Cache Hit Stats', '返回缓存命中统计'),
    ('enforce_eager', False, 'performance', 'switch', True, 'Enforce Eager Mode', '强制 Eager'),
    ('lora', False, 'model', 'switch', False, 'Enable LoRA', '启用 LoRA'),
)
ENGINE = {
    'vllm': (
        ('enable_chunked_prefill', None, 'performance', 'switch', True, 'Chunked Prefill', '分块预填充'),
        ('enable_request_metrics', True, 'performance', 'switch', True, 'Per-request Timings', '返回逐请求耗时'),
        ('kv_disk_offload', False, 'performance', 'tier', True, 'KV Offload to Disk', 'KV Offload 到磁盘'),
        ('async_scheduling', False, 'performance_advanced', 'switch', False, 'Asynchronous Scheduling', '异步调度'),
    ),
    'sglang': (
        ('enable_metrics', True, 'performance', 'switch', True, 'Enable Inference Metrics', '推理监控指标'),
    ),
}
# The owned vLLM images have no recorded engine release. Thus 0.25-gated
# request metrics is engine-decided even when the fixture explicitly states on.
# SGLang's explicit serve entrypoint accepts its metrics default.
ENGINE_DECIDES = {'vllm': ('enable_request_metrics',), 'sglang': ()}
TRISTATE = {
    'vllm': ('enable_chunked_prefill', 'enable_prefix_caching', 'enable_request_metrics'),
    'sglang': ('enable_prefix_caching',),
}
# DeployQuickForm first calls quickParamSpecs (profile.quick_form_keys), then
# applies layout. Neither chunked prefill nor request metrics is in vLLM's
# whitelist. Expert Performance reads the full performance group directly.
SURFACES = {
    'vllm': {'quick': ('enable_prefix_caching',), 'expert': TRISTATE['vllm']},
    'sglang': {'quick': ('enable_prefix_caching',), 'expert': TRISTATE['sglang']},
}
QUICK_BOOLEAN_KEYS = {
    'vllm': ('trust_remote_code', 'enable_auto_tool_choice', 'enable_prefix_caching',
             'enable_cache_report', 'kv_disk_offload', 'enforce_eager'),
    'sglang': ('enable_metrics', 'trust_remote_code', 'enable_auto_tool_choice',
               'enable_prefix_caching', 'enable_cache_report', 'enforce_eager'),
}
POSITIONS = ('on', 'off', 'auto')


def same(actual, expected, label):
    dump = lambda value: json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)
    check(dump(actual) == dump(expected), f'{label}: expected={expected!r}; actual={actual!r}')


def reviewed_profile(profile):
    engine = profile['name']
    check(engine in ENGINE, 'boolean contract has no reviewed engine')
    expected = {row[0]: row for row in COMMON + ENGINE[engine]}
    actual = {spec['key']: spec for spec in profile['param_specs'] if spec['type'] == 'boolean'}
    same(sorted(actual), sorted(expected), 'boolean complete dictionary universe')
    for key, row in expected.items():
        spec = actual[key]
        descriptor = {'default': spec.get('default'), 'group': spec.get('group'),
            'control': spec.get('control'), 'quick': spec.get('layout') == 'quick',
            'engines': spec.get('engines'), 'multimodal_only': spec.get('multimodal_only', False),
            'label': spec.get('label'), 'depends_on': spec.get('depends_on')}
        same(descriptor, {'default': row[1], 'group': row[2], 'control': row[3],
            'quick': row[4], 'engines': [engine], 'multimodal_only': False,
            'label': {'en': row[5], 'zh': row[6]},
            'depends_on': 'kv_offloading_size' if key == 'kv_disk_offload' else None},
            'boolean reviewed schema ' + key)
        if row[1] is not None:
            check(type(spec.get('default')) is bool, 'boolean default is not a JSON boolean: ' + key)
    same(sorted(set(profile['quick_form_keys']) & set(expected)),
        sorted(QUICK_BOOLEAN_KEYS[engine]), 'boolean reviewed Quick whitelist')
    return expected


def evaluate_body(body):
    """Independent evaluate/create distinction, documented by the six fixtures.

    Evaluate has no display name. Automatic evaluation does not pin the node
    or submit's selected GPU/TP; those only enter the successful create body.

    It also carries no quick_params until the operator authors one. ADR 0020 rule
    3: during reevaluation, values the server projected and nobody edited do not
    become explicit fields, so a default or an earlier template cannot override a
    newly selected one. EvaluateQuickParams.prepare deletes the key when nothing
    was authored (evaluateQuickParams.ts:36) and create still submits the complete
    displayed set, which is what the fixture body describes. Callers that edit a
    key add it back through position_body, so the explicit path stays exact.
    """
    expected = copy.deepcopy(body)
    expected.pop('name')
    expected.pop('quick_params', None)
    if expected['placement_mode'] == 'auto':
        expected.pop('node_uuid')
        expected.pop('placement')
    return expected



def evaluate_body_after_edits(body, edited_keys):
    """evaluate_body, plus the quick values this case authored by editing.

    ADR 0020: an edited key stays explicit for the rest of the session, and it
    travels with the value it was restored to. A case that walked every control
    and then restored the fixture therefore ends on a body that names those keys
    with the fixture's own values - still exactly the fixture's configuration,
    which is what the closing checkpoint is for, but not the keyless body a
    pristine form sends. Keys the case never touched stay absent.
    """
    expected = evaluate_body(body)
    authored = {key: body['quick_params'][key]
                for key in edited_keys if key in body.get('quick_params', {})}
    if authored:
        expected['quick_params'] = authored
    else:
        expected.pop('quick_params', None)
    return expected


def position_body(body, key, position, marker):
    check(key in TRISTATE[body['engine']], 'unreviewed tri-state key')
    check(position in POSITIONS, 'unreviewed tri-state position')
    expected = evaluate_body(body)
    expected['served_name'] = marker
    # Setting this control is an authored edit, so it is the one quick value that
    # must travel. Auto states no value, and EvaluateQuickParams drops the key
    # entirely rather than sending an empty object, so the expected body has no
    # quick_params at all on that path.
    if position == 'auto':
        expected.pop('quick_params', None)
    else:
        expected['quick_params'] = {key: position == 'on'}
    return expected


def assert_wire(actual, expected, key, position, label):
    """Reject false/0, true/1, omission/null and unexpected explicit unset."""
    params = actual.get('quick_params', {})
    check(type(params) is dict, label + ': quick_params must be object')
    if position == 'auto':
        check(key not in params, label + ': Auto must omit key')
    else:
        check(key in params and type(params[key]) is bool, label + ': choice is not a JSON boolean')
        same(params[key], position == 'on', label + ': wrong boolean polarity')
    same(actual, expected, label + ' complete evaluate wire')
