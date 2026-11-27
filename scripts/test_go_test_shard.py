"""Exercise the CI shard splitter offline: partition, balance and refusals."""

import os
import pathlib
import subprocess
import sys
import tempfile
import textwrap
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SHARD = ROOT / "scripts" / "go_test_shard.sh"

TESTS = [
    "TestAlpha", "TestBravo", "TestCharlie", "TestDelta", "TestEcho",
    "TestFoxtrot", "TestGolf", "TestHotel", "TestIndia", "TestJuliet",
    "TestKilo", "TestLima",
]

# One heavy case and a long tail: round-robin by name cannot see this, a
# duration-weighted split can.
WEIGHTS = textwrap.dedent("""\
    # measured on one runner
    TestAlpha 300
    TestBravo 120
    TestCharlie 60
    TestDelta 30
    """)


class ShardTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="gpuworks-shard-")
        self.root = pathlib.Path(self.tmp.name)
        fake = self.root / "go"
        fake.write_text("#!" + sys.executable + "\n" + textwrap.dedent("""\
            import sys
            if sys.argv[1:3] == ["test", "-list"]:
                print("ok  \\tgpuworks.fixture\\t0.001s")
                for name in %r:
                    print(name)
                sys.exit(0)
            sys.exit("unexpected go invocation: %%r" %% (sys.argv,))
            """ % (TESTS,)))
        fake.chmod(0o755)
        self.env = {**os.environ, "PATH": str(self.root) + os.pathsep + os.environ["PATH"]}

    def tearDown(self):
        self.tmp.cleanup()

    def shard(self, index, total, weights=None):
        env = dict(self.env)
        if weights is None:
            env.pop("GPUWORKS_SHARD_WEIGHTS", None)
        else:
            path = self.root / "weights.txt"
            path.write_text(weights)
            env["GPUWORKS_SHARD_WEIGHTS"] = str(path)
        result = subprocess.run(["sh", str(SHARD), "%d/%d" % (index, total),
                                 "-tags=multiproc", "./test/l2/..."],
                                env=env, capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def names(self, pattern):
        self.assertTrue(pattern.startswith("^(") and pattern.endswith(")$"), pattern)
        return pattern[2:-2].split("|")

    def split(self, total, weights=None):
        return [set(self.names(self.shard(index, total, weights))) for index in range(1, total + 1)]

    def test_names_partition_every_test_once(self):
        shards = self.split(3)
        union = set().union(*shards)
        self.assertEqual(union, set(TESTS), "every test must land in exactly one shard")
        self.assertEqual(sum(len(shard) for shard in shards), len(TESTS), "no test may be in two shards")

    def test_round_robin_without_weights_spreads_by_position(self):
        shards = self.split(3)
        self.assertEqual([len(shard) for shard in shards], [4, 4, 4])

    def test_weights_bound_the_heaviest_shard(self):
        weights = {line.split()[0]: float(line.split()[1])
                   for line in WEIGHTS.splitlines() if line and not line.startswith("#")}
        shards = self.split(3, WEIGHTS)
        self.assertEqual(set().union(*shards), set(TESTS))
        self.assertEqual(sum(len(shard) for shard in shards), len(TESTS))
        load = [sum(weights.get(name, 0) for name in shard) for shard in shards]
        total = sum(weights.get(name, 0) for name in TESTS)
        heaviest = max(weights, key=weights.get)
        # No split beats a single test that outweighs a fair share of the suite,
        # so the floor is that test's own duration rather than total/3.
        floor = max(total / 3, weights[heaviest])
        self.assertLessEqual(max(load), floor + 60,
                             "heaviest shard %r must stay near max(third, heaviest) of %r" % (load, total))
        self.assertEqual(sum(1 for shard in shards if heaviest in shard), 1)

    def test_unknown_tests_weigh_nothing_and_are_still_sharded(self):
        shards = self.split(2, WEIGHTS)
        self.assertEqual(set().union(*shards), set(TESTS))
        self.assertEqual(sum(len(shard) for shard in shards), len(TESTS))

    def test_malformed_weights_are_refused(self):
        for body in ("TestAlpha 3x\n", "TestAlpha\n", "TestAlpha 1 2\n"):
            path = self.root / "weights.txt"
            path.write_text(body)
            result = subprocess.run(["sh", str(SHARD), "1/2", "./test/l2/..."],
                                    env={**self.env, "GPUWORKS_SHARD_WEIGHTS": str(path)},
                                    capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 2, "%r was accepted: %s" % (body, result.stdout))

    def test_shard_out_of_range_is_refused(self):
        result = subprocess.run(["sh", str(SHARD), "4/3", "./test/l2/..."],
                                env=self.env, capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 2)


if __name__ == "__main__":
    unittest.main()
