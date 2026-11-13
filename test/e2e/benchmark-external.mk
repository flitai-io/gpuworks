E2E_LOCALE ?= en

.PHONY: test-e2e-benchmark-fixture test-e2e-benchmark-external test-e2e-benchmark-cancel test-e2e-benchmark-selection
## The fixture is an auditable OpenAI-compatible external boundary; the server,
## SQLite, executor and browser stay real. No GPU or container is required.
test-e2e-benchmark-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_benchmark_fixture test_benchmark_evidence test_benchmark_support -v

test-e2e-benchmark-external: build test-e2e-benchmark-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/evaluation_external.py --locale $(E2E_LOCALE) --case sweep

test-e2e-benchmark-cancel: build test-e2e-benchmark-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/evaluation_external.py --locale $(E2E_LOCALE) --case cancel

test-e2e-benchmark-selection: build test-e2e-benchmark-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/evaluation_bulk_selection.py --locale $(E2E_LOCALE) --case selection
