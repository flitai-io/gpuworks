"""Phase assignment covers every open checklist gap.

Run: PYTHONPATH=test/e2e:scripts python3 -m unittest test_coverage_completion_queue
"""
import unittest

from coverage_completion_queue import summary

# Baseline on wei/master 64fe95b6b, after wizard 12.1/12.2/12.3/12.5,
# instance-diagnostics 12.2/12.3/12.4/12.5 with 12.8 N/A,
# settings-ssh-key 12.1–12.4 with 12.5 and 12.8 N/A,
# platform-token-read-revoke 12.1–12.4 with 12.5 and 12.8 N/A,
# inference-key-lifecycle 12.1–12.4 with 12.5 and 12.8 N/A,
# storage-local 12.1–12.4 with 12.5 N/A,
# network-type-roles 12.1–12.4 with 12.5 and 12.8 N/A,
# platform-token-options 12.1–12.4 with 12.5 and 12.8 N/A,
# network-storage-lifecycle networks 12.1–12.4 with 12.5 and 12.8 N/A
# plus storage 12.1–12.4 with 12.5 N/A,
# http-model-registration 12.1–12.4 and 12.6 with 12.5 N/A,
# assistant-service-tools 12.1–12.4 with 12.5 and 12.8 N/A, and
# storage-nfs-cache-policy 12.2/12.3 covered with 12.5 N/A
# (12.1 was already covered; 12.4/12.6/12.7/12.8 stay MISSING), and
# inference-key-lifecycle 12.6 covered (12.7 stays partial), and
# platform-token-read-revoke 12.6 covered (12.7 stays partial), and
# network-type-roles 12.6 covered (12.7 stays partial), and
# assistant-service-tools 12.6 covered (12.7 stays MISSING), and
# network-storage-lifecycle networks 12.6 covered (storage 12.6 and network 12.7 stay MISSING), and
# platform-token-options 12.6 covered (12.7 stays MISSING), and
# storage-local 12.6 covered (12.8 stays MISSING), and
# network-storage-lifecycle storage 12.6 covered (12.7 and 12.8 stay MISSING), and
# storage-nfs-cache-policy 12.6 covered (12.4, 12.7 and 12.8 stay MISSING), and
# storage-local 1.9 covered, and
# platform-token-options 1.9 covered (12.7 stays MISSING), and
# platform-token-read-revoke 1.9 covered (12.7 stays partial), and
# inference-key-lifecycle 1.9 covered (12.7 stays partial), and
# assistant-service-tools 1.9 covered (12.7 stays MISSING), and
# storage-nfs-cache-policy 1.9 covered (12.4, 12.7 and 12.8 stay MISSING), and
# settings-branding 1.9 covered, and
# settings-telemetry 1.9 covered, and
# settings-deployment-policy 1.9 covered (1.4 stays partial), and
# settings-ssh-key 1.9 covered, and
# provider-endpoint-edit 1.9 covered (other section 1 rows stay partial or missing), and
# settings-branding 1.8 covered, and
# provider-endpoint-edit 1.10 covered (other section 1 rows stay partial or missing), and
# settings-branding 1.2 covered, and
# settings-telemetry 1.2 covered, and
# settings-ssh-key 1.2 covered, and
# network-type-roles 1.14 covered (1.1, 1.9 and 1.10 stay in the partial slash key), and
# inference-key-lifecycle 1.11 covered (other partial rows stay partial), and
# platform-token-read-revoke 1.11 covered (1.2 stays partial), and
# http-model-registration 1.11 covered (other partial rows stay partial), and
# settings-ssh-key 1.11 covered, and
# storage-local 1.11 covered (1.5 and 1.6 stay partial), and
# assistant-service-tools 1.11 covered (12.7 stays MISSING), and
# platform-token-options 1.11 covered (12.7 stays MISSING), and
# network-type-roles 1.11 covered (1.1, 1.9 and 1.10 stay in the partial slash key), and
# network-storage-lifecycle networks 1.11 covered (other section 1 rows stay partial), and
# network-storage-lifecycle storage 1.11 covered (other section 1 rows stay partial), and
# inference-key-lifecycle 1.10 and 1.14 covered (other partial rows stay partial), and
# platform-token-read-revoke 1.10 covered (1.2 stays partial), and
# platform-token-options 1.10 covered (12.7 stays MISSING), and
# assistant-service-tools 1.10 covered (12.7 stays MISSING), and
# nodes-labels-search 12.6 covered (other section 12 rows stay undeclared), and
# template-edit 12.6 covered (12.1-12.5 and 12.7 stay partial, 12.8 stays undeclared), and
# node-removal 12.6 covered (12.2, 12.4 and 12.7 stay partial), and
# image-catalog-archive-prefill 12.6 covered (12.1 and 12.2 stay partial), and
# image-catalog-community-prefill 12.6 covered (12.1 and 12.2 stay partial), and
# image-catalog-register 12.6 covered (12.1, 12.2 and 12.4 stay partial), and
# model-hub-huggingface 12.6 covered (12.7 and 12.8 stay missing), and
# model-hub-modelscope 12.6 covered (12.7 and 12.8 stay missing), and
# model-hub-quick 12.6 covered (12.7 and 12.8 stay missing), and
# models-local-register 12.6 covered (other section 12 rows stay undeclared), and
# image-runtime-failure-retry 12.6 covered (12.1 and 12.2 stay partial, other section 12 rows stay undeclared), and
# model-files-failure-retry 12.6 covered (other section 12 rows stay missing), and
# deployment-container-overrides 12.6 covered (other section 12 rows stay missing), and
# deployment-wizard-create 12.6 covered (12.4, 12.7 and 12.8 stay missing), and
# deployment-acquisition-vllm-huggingface 12.6 covered (other section 12 rows stay missing), and
# deployment-acquisition-vllm-modelscope 12.6 covered (other section 12 rows stay missing), and
# deployment-acquisition-sglang-huggingface 12.6 covered (other section 12 rows stay missing), and
# deployment-acquisition-sglang-modelscope 12.6 covered (other section 12 rows stay missing), and
# deployment-health 12.6 covered (other section 12 rows stay missing), and
# deployment-replicas-scale 12.6 covered (other section 12 rows stay missing), and
# deployment-replicas-drain 12.6 covered (other section 12 rows stay missing), and
# deployment-replicas-immediate 12.6 covered (other section 12 rows stay missing), and
# model-files-download-delete 12.6 covered (other section 12 rows stay missing), and
# model-files-download-delete 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing or partial), and
# deployment-replicas-immediate 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# deployment-replicas-drain 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# model-files-failure-retry 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# model-files-interrupted-recovery 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.6 and 12.7 stay missing), and
# model-hub-huggingface 12.1, 12.2 and 12.3 covered (12.4 and 12.5 stay missing), and
# model-hub-modelscope 12.1, 12.2 and 12.3 covered (12.4 and 12.5 stay missing), and
# model-hub-quick 12.1, 12.2 and 12.3 covered (12.4, 12.5 and 12.7 stay missing), and
# image-runtime-failure-retry 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay undeclared), and
# image-runtime-cancel-retry 12.1, 12.2 and 12.3 covered (12.4 and 12.6 stay undeclared), and
# models-local-register 12.1 and 12.3 covered (the existing 12.2 sentence stays, 12.4 stays undeclared), and
# nodes-labels-search 12.1 and 12.3 covered (the existing 12.2 sentence stays, 12.4 stays undeclared), and
# node-removal 12.2 and 12.3 covered (12.1 stays in its existing binding, 12.4 stays partial), and
# deployment-container-overrides 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# deployment-acquisition-vllm-huggingface 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# deployment-acquisition-vllm-modelscope 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# deployment-acquisition-sglang-huggingface 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing), and
# deployment-acquisition-sglang-modelscope 12.1, 12.2 and 12.3 covered (12.4, 12.5, 12.7 and 12.8 stay missing).
# node-removal 12.7 covered (12.4 stays partial).
# http-model-registration 12.7 covered (12.8 stays missing).
# model-hub-quick 12.7 covered (12.4, 12.5 and 12.8 stay missing).
# model-hub-huggingface 12.7 covered (12.4, 12.5 and 12.8 stay missing).
# model-hub-modelscope 12.7 covered (12.4, 12.5 and 12.8 stay missing).
# network-storage-lifecycle storage 12.7 covered (the network 12.7 stays missing).
# model-files-download-delete 12.7 covered.
# models-local-register 12.7 covered (12.5 and 12.8 stay undeclared).
# models-local-register 12.4 covered.
# model-files-failure-retry 12.7 covered.
# model-files-interrupted-recovery 12.7 covered (12.4, 12.5 and 12.8 stay missing).
# template-edit 12.5 covered (12.1-12.4 and 12.7 stay partial).
# image-runtime-failure-retry 12.4 and 12.5 covered.
# image-runtime-cancel-retry 12.4 and 12.5 covered.
# deployment-replicas-drain 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# deployment-replicas-immediate 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# model-files-download-delete 12.4 covered (12.5 stays missing).
# model-files-failure-retry 12.4 covered (12.5 stays missing).
# model-files-interrupted-recovery 12.4 covered (12.5 stays missing).
# model-files-interrupted-recovery 12.6 covered (12.5 stays missing).
# storage-nfs-cache-policy 12.8 covered (12.4 and 12.7 stay missing).
# model-hub-huggingface 12.4 covered (12.5 and 12.8 stay missing).
# model-hub-modelscope 12.4 covered (12.5 and 12.8 stay missing).
# image-runtime-cancel-retry 12.6 covered (12.7 and 12.8 stay undeclared).
# instance-diagnostics 12.6 covered (12.7 stays missing).
# storage-local 12.8 covered.
# image-catalog-register, archive-prefill and community-prefill 12.1 and 12.3 covered
# (12.2 stays partial; 12.4 stays partial on register).
# template-edit 12.1 covered (12.2, 12.4 and 12.7 stay partial).
# template-edit 12.3 covered (12.2, 12.4 and 12.7 stay partial).
# image-catalog-enabled-cycle 12.1 and 12.3 covered (12.2 and 12.4 stay partial).
# node-reconnect-doctor 12.1 covered (12.4-12.8 stay undeclared).
# node-reconnect-doctor 12.3 covered (12.4-12.8 stay undeclared).
# template-edit 12.7 covered (12.2 and 12.4 stay partial).
# deployment-replicas-scale 12.1 and 12.3 covered (12.2, 12.4, 12.5, 12.7 and 12.8 stay missing).
# node-reconnect-doctor 12.4 and 12.5 covered (12.6, 12.7 and 12.8 stay undeclared).
# deployment-health 12.3 covered (12.2, 12.4, 12.5, 12.7 and 12.8 stay missing).
# network-storage-lifecycle storage 12.8 covered (network 12.8 stays N/A).
# deployment-health 12.4 and 12.5 covered (12.2, 12.7 and 12.8 stay missing).
# deployment-replicas-scale 12.4 and 12.5 covered (12.2, 12.7 and 12.8 stay missing).
# deployment-container-overrides 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# deployment-acquisition-vllm-huggingface 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# deployment-acquisition-vllm-modelscope 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# deployment-acquisition-sglang-huggingface 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# deployment-acquisition-sglang-modelscope 12.4 and 12.5 covered (12.7 and 12.8 stay missing).
# models-local-register 1.9, 1.10 and 1.11 covered (other section 1 rows stay undeclared; 12.5 and 12.8 stay undeclared).
# model-files download-delete and interrupted-recovery 1.9, 1.10 and 1.11 covered (failure-retry 1.10 stays partial).
# model-files-failure-retry 1.9 and 1.11 covered.
# model-hub-huggingface 1.9 and 1.11 covered (1.10 stays covered).
# model-hub-modelscope 1.9 and 1.11 covered (1.10 stays missing).
# model-hub-quick 1.9 and 1.10 covered (1.11 stays missing).
# nodes-labels-search 1.9 and 1.10 covered (1.12 stays partial).
# image-catalog-register, image-catalog-community-prefill and image-catalog-archive-prefill 1.9 and 1.10 covered (1.11 stays partial).
# network-type-roles 1.9 and 1.10 covered (1.1 stays partial).
# deployment-health, replicas scale/drain/immediate 1.9 and 1.10 covered.
# deployment-container-overrides and deployment-wizard-create 1.9 covered.
# four deployment-acquisition contracts 1.9 and 1.10 covered.
# network-storage-lifecycle networks and storage 1.9 and 1.10 covered (the remaining section 1 slash stays partial).
# http-model-registration recovery 1.9 covered (register scenario does not declare 1.9).
# settings-branding 2.4 covered.
# settings-branding 2.3 covered.
# settings-deployment-policy 3.2 covered.
# settings-deployment-policy 3.3 covered.
# settings-telemetry 3.2 covered.
# settings-branding 2.6 covered.
# settings-telemetry 3.3 covered.
# storage-nfs-cache-policy 3.2 covered.
# storage-nfs-cache-policy 3.3 covered.
# nodes-labels-search 2.2 covered.
# nodes-labels-search 2.3 covered.
# nodes-labels-search 2.6 covered.
# storage-nfs-cache-policy 3.1 covered.
# nodes-labels-search 1.1 covered.
# nodes-labels-search 1.2 covered.
# nodes-labels-search 1.7 covered.
# nodes-labels-search 1.13 covered.
# settings-deployment-policy 1.1 covered.
# settings-telemetry 1.1 covered.
# settings-telemetry 1.12 covered.
# settings-telemetry 1.15 covered.
# settings-telemetry 1.13 covered.
# settings-deployment-policy 1.13 covered.
# settings-telemetry 1.8 covered.
# nodes-labels-search 1.11 covered.
# settings-branding 1.1 covered.
# settings-ssh-key 1.1 covered.
# settings-branding 1.13 covered.
# settings-ssh-key 1.13 covered.
# platform-token-options 1.3 covered.
# platform-token-options 1.7 covered.
# nodes-labels-search 5.4 covered.
# platform-token-options 1.1 covered.
# inference-key-lifecycle 1.1 covered.
# platform-token-read-revoke 1.1 covered.
# assistant-service-tools 1.15 covered.
# nodes-labels-search 1.15 covered.
# network-type-roles 1.3 covered.
# settings-ssh-key 1.15 covered.
# storage-local 1.3 covered.
# session-logout 1.14 covered.
# image-catalog-archive-prefill 1.14 covered.
# provider-endpoint-edit 1.2 covered.
# settings-deployment-policy 1.15 covered.
# assistant-service-tools 1.1 covered.
# storage-local 1.1 covered.
# image-catalog-archive-prefill 1.1 covered.
# image-catalog-community-prefill 1.1 covered.
# image-catalog-register 1.1 covered.
# session-logout 1.1 covered.
# provider-endpoint-edit 1.1 covered.
# network-type-roles 1.1 covered.
# network-storage-lifecycle network 1.1 covered.
# network-storage-lifecycle storage 1.1 covered.
# http-model-registration 1.1 covered.
# model-hub-huggingface 1.1 covered.
# storage-nfs-cache-policy 1.1 covered.
# storage-nfs-cache-policy 1.3 covered.
# settings-ssh-key 1.7 covered.
# settings-telemetry 1.7 covered.
# model-hub-modelscope 1.1 covered.
# model-hub-quick 1.1 covered.
# session-logout 1.7 covered.
# dashboard-fleet 1.1 covered.
# nodes-labels-search 1.8 covered.
# network-type-roles 1.7 covered.
# network-type-roles 1.2 covered.
# storage-local 1.2 covered.
# network-type-roles 1.15 covered.
# nodes-labels-search 1.12 covered.
# settings-telemetry 1.10 covered.
# storage-local 1.12 covered.
# network-type-roles 1.8 covered.
# storage-local 1.8 covered.
# storage-local 1.13 covered.
# image-catalog-archive-prefill 1.3 covered.
# image-catalog-community-prefill 1.3 covered.
# image-catalog-community-prefill 1.14 covered.
# image-catalog-archive-prefill 1.7 covered.
# image-catalog-community-prefill 1.7 covered.
# image-catalog-community-prefill 1.8 covered.
# image-catalog-archive-prefill 1.8 covered.
# image-catalog-register 1.14 covered.
# image-catalog-register 1.7 covered.
# image-catalog-register 1.8 covered.
# image-catalog-register 1.3 covered.
# image-catalog-register 1.15 covered.
# http-model-registration 1.7 covered.
# http-model-registration 1.8 covered.
# http-model-registration 1.14 covered.
# models-local-register 1.1 covered.
# session-logout 1.10 covered.
# settings-branding 1.10 covered.
# settings-branding 1.15 covered.
# provider-endpoint-edit 1.15 covered.
# provider-endpoint-edit 1.11 covered.
# session-logout 1.9 covered.
# benchmark-external-sweep 1.9 covered.
# benchmark-external-sweep 1.15 covered.
# storage-nfs-cache-policy 1.11 covered.
# storage-nfs-cache-policy 1.15 covered.
# settings-branding 1.12 covered.
# network-type-roles 1.13 covered.
# platform-token-read-revoke 1.8 covered.
# assistant-service-tools 1.8 covered.
# models-local-register 1.8 covered.
# model-hub-huggingface 1.8 covered.
# model-hub-modelscope 1.8 covered.
# model-hub-quick 1.7 covered.
# model-hub-quick 1.11 covered.
# model-hub-quick 1.15 covered.
# model-hub-huggingface 1.7 covered.
# model-hub-modelscope 1.7 covered.
# model-hub-modelscope 1.10 covered.
# model-hub-huggingface 1.14 covered.
# model-hub-modelscope 1.14 covered.
# model-hub-quick 1.14 covered.
# settings-deployment-policy 1.12 covered.
# storage-nfs-cache-policy 1.10/1.13 covered; 3.3 downgraded for fractional unit display.
# deployment wizard/overrides/acquisition 2.7/13.5; acquisition 1.13; row entry stays partial.
# Replica refusal recovery1.10/12.6 downgraded: SQLite state injection bypasses controlplane.
# Logs2.2/2.6/9.4/18.8 verified by frozen mutantFAIL and en/zh allPASS.
# Sixdeployment selector8.1 consumers verified independently in bothlocales.
# Provider1.7/1.13/2.3/2.6/2.7/7.1 verified;2.1 stays partial; residual section now7.
# Provider nine new RunPod/conditional/URL/secret items and storage3.3/3.4 verified all en/zh; providers24/30.
# Six deployment contracts14.3/14.7/15.4/15.6 verified by same-helper mutation FAIL and all en/zh PASS.
# Six deployment editor15.1/tri-state6.3 contracts verified by12 targeted faults and12 restored all en/zh runs.
# Six complete current-fixture boolean6.1 inventories verified against unfixed Expert baseline and restored all en/zh.
# These counts move only when a manifest declaration changes.
BASELINE = {
    "1": 124, "2": 251, "3": 249, "4": 318, "5": 189, "6": 130,
    "7": 101, "8": 89, "9": 65, "10": 95, "11": 83, "out": 297,
}

# The deploy manifests declare section 17 not applicable: no deploy page imports a chart library,
# and a recursive search of ui/src/pages/deploy/ finds no echarts, Statistic, Timeline, Progress or
# canvas, so the seven chart items leave the queue on six manifests at once (42 fewer open items).
#
# The container-overrides manifest also leaves the queue on sections 13 and 22: 13.1 to 13.3 and
# 22.1, 22.2 and 22.7 are measured, 13.4 and 13.6 name a branch the product's own data keeps shut
# (every image series is official, PATCH answers 400 and there is no create route), and 22.3 is a
# single-role product that cannot produce the second identity it would compare against. Three
# drafts had also cleared 22.4 to 22.6 as not applicable; those items describe a session timeout,
# a locale switcher and theme tokens the product does have, so the sentences were about the
# contract rather than the product and the items are open again.


class QueueTest(unittest.TestCase):
    def test_every_open_checklist_gap_has_one_phase(self):
        extra, counts, total = summary()
        # The provider tables carry no row selection, so 11.5 to 11.7 are declared not applicable on
        # that surface with the measured header and checkbox count as the reason; with them resolved,
        # section 11 no longer has an open provider residual to route to a phase.
        self.assertIsNone(extra)
        self.assertEqual(dict(counts), BASELINE)
        self.assertEqual(total, sum(BASELINE.values()))
        self.assertEqual(total, 1991)


if __name__ == "__main__":
    unittest.main()
