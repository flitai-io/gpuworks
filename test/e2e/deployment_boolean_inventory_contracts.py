"""Source-reviewed per-surface inventory, independent of DOM discovery.

QuickForm quickParamSpecs -> layout -> feature/advanced; Expert Model and
Performance use visible groups. HealthCheckFields, TokenizeField,
DeployResourceStep and ContainerOverridesEditor supply static controls.
"""
from deployment_boolean_contracts import COMMON, ENGINE, ENGINE_DECIDES, same
from platform_token_read_revoke import check

QUICK = {
    'trust_remote_code': 'advanced-switch', 'enable_auto_tool_choice': 'advanced-switch',
    'enable_prefix_caching': 'feature-select', 'enable_cache_report': 'feature-select',
    'enforce_eager': 'feature-select', 'lora': 'absent',
    'enable_metrics': 'feature-switch', 'enable_chunked_prefill': 'absent',
    'enable_request_metrics': 'absent', 'kv_disk_offload': 'tier-choice',
    'async_scheduling': 'absent',
}
SLEEP_SUPPORTED = {'vllm': True, 'sglang': False}
# Shared six-contract bootstrap: FixedProber's A100x1,24576 is NVIDIA and
# synthetic; NO_INJECT affects dispatch only. Synthetic runtime validation
# skips host plugins; conn declares safety protocol1, embedded libvgpu passes
# validation and NVIDIA skips the behavior probe. Thus quota is available.
FIXTURE_GPU = {'index': 0, 'uuid': 'GPU-FAKE-0', 'name': 'A100',
               'vendor': 'nvidia', 'memory_total_bytes': 24576 * 2**20}
FIXTURE_PARTITION = {'backend': 'nvidia-dgpu', 'vendor': 'nvidia', 'status': 'ready'}
STATIC = ('global/health_check', 'global/tokenize_enabled', 'quick/sleepable',
          'quick/partition-capability', 'quick/expert-parallel-capability',
          'expert/resource/sleepable', 'expert/performance/memory-tier',
          'quick/mount-rows', 'expert/runtime/mount-rows', 'conditional/mount-read-only')


CARRIED_MARK = '__carried_quick'
_MISSING = object()


def rows(engine):
    check(engine in ENGINE, 'unreviewed inventory engine')
    return {row[0]: row for row in COMMON + ENGINE[engine]}


def expected_ids(engine):
    # Absent controls remain required matrix entries with a source reason.
    return ['dictionary/' + surface + '/' + key for surface in ('quick', 'expert')
            for key in rows(engine)] + list(STATIC)


def initial(body, key):
    row = rows(body['engine'])[key]
    # Two different questions arrive here. A pristine inventory asks what the
    # fixture states and what the engine decides; a checkpoint after real edits
    # asks what the page is showing now, and those carried values are marked.
    # Merging the two without the mark made the inventory expect an On control
    # for an engine-decided key the page had never been asked to set.
    explicit = (body.get(CARRIED_MARK) or {}).get(key, _MISSING)
    if explicit is not _MISSING:
        return {'position': 'on' if explicit else 'off', 'wire': 'explicit', 'value': explicit}
    if key in ENGINE_DECIDES[body['engine']]:
        return {'position': 'auto', 'wire': 'omitted', 'value': None}
    if key in body['quick_params']:
        value = body['quick_params'][key]
        check(type(value) is bool, 'fixture boolean is not boolean: ' + key)
        return {'position': 'on' if value else 'off', 'wire': 'explicit', 'value': value}
    if row[1] is None:
        # No stated value and no dictionary default: the control states no
        # choice. A default of None is not the same thing as being engine
        # decided, so this check has to come after the explicit one.
        return {'position': 'auto', 'wire': 'omitted', 'value': None}
    return {'position': 'on' if row[1] else 'off', 'wire': 'omitted', 'value': row[1]}


def complete(records, engine):
    actual = [record['id'] for record in records]
    check(len(actual) == len(set(actual)), 'boolean inventory duplicate field observations')
    same(sorted(actual), sorted(expected_ids(engine)), 'boolean inventory complete field/surface matrix')
    for record in records:
        check(type(record.get('offered')) is bool, record['id'] + ': missing offered observation')
        if not record['offered']:
            check(bool(record.get('source_reason')), record['id'] + ': absence has no reviewed source reason')


def fixture_hardware(node, node_uuid):
    same({key: node.get(key) for key in ('uuid', 'gpu_count', 'gpu_vendor', 'runtime_driver')},
         {'uuid': node_uuid, 'gpu_count': 1, 'gpu_vendor': 'nvidia', 'runtime_driver': 'docker'},
         'boolean inventory independent fixture node/runtime facts')
    same([{key: gpu.get(key) for key in FIXTURE_GPU} for gpu in node['gpus']],
         [FIXTURE_GPU], 'boolean inventory independent A100x1,24576 GPU facts')
    same([{key: capability.get(key) for key in FIXTURE_PARTITION}
          for capability in node['gpus'][0].get('partition_capabilities', [])],
         [FIXTURE_PARTITION], 'boolean inventory independent embedded NVIDIA capability')


def health_source(body):
    """Fixed registration + effective-spec contract, never DOM/API inference.

    Local fixture registration seeds Qwen text-generation -> openai.chat.
    Template quick-register intentionally leaves operations empty and metadata
    refresh preserves that contract. All six imported specs omit api; general
    is presentation only, so effectiveTemplateAPIContract supplies no operation.
    initTemplateModelInferenceConfig therefore adds none at create time.
    """
    env = body['container_overrides']['env']
    local, acquisition = 'E2E_WIZARD' in env, 'E2E_ACQUISITION' in env
    check(local != acquisition, 'unreviewed health fixture registration identity')
    return {'registration': 'local Qwen OperationsForTasks' if local else 'template quick-register',
            'raw_operations': ['openai.chat'] if local else [], 'spec_operations': [],
            'effective_operations': ['openai.chat'] if local else [],
            'expected_health': local, 'threshold_default': 60}


def health_contract(body, model, template):
    expected = health_source(body)
    same({'id': model['id'], 'source': model['source']},
         {'id': body['model_id'], 'source': 'local' if expected['expected_health'] else body['model_source']['provider']},
         'inventory fixed registered model identity/source')
    check(type(model.get('operations')) is list, 'inventory model operations must be a JSON array')
    same([operation['type'] for operation in model['operations']], expected['raw_operations'],
         'inventory fixed raw registered model operations')
    same(template['id'], body['template_id'], 'inventory fixed template identity')
    specs = [spec for spec in template['specs'] if spec['name'] == body['template_spec_name']
             and spec['source'] == body['template_spec_source']]
    check(len(specs) == 1, 'inventory fixed owned template spec identity')
    spec = specs[0]
    # Migration 197 replaced display_profile (general/compatible/optimized) with two
    # human-written names; the server rejects the retired key outright. What this line
    # still has to prove is that the name the fixture wrote is the name the API returns,
    # so it compares the two names against each other instead of pinning a literal that
    # only one of the fixtures uses.
    check(bool(spec.get('display_name_en')), 'inventory fixture lost its English display name')
    check(bool(spec.get('display_name_zh_cn')), 'inventory fixture lost its Chinese display name')
    same({'health_endpoint': spec['api'].get('health_endpoint'),
          'operations': spec['api'].get('operations')},
         {'health_endpoint': '/health', 'operations': []},
         'inventory effective undeclared template API operations')
    return expected
