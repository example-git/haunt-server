import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('regression_results', Path(__file__).parents[1] / 'check-test-results.py')
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class RegressionResultTests(unittest.TestCase):
    def check(self, events):
        with tempfile.TemporaryDirectory() as temporary:
            filename = Path(temporary) / 'results.jsonl'
            filename.write_text(''.join(json.dumps(event) + '\n' for event in events))
            checker.check(filename)

    def test_missing_database_is_not_a_green_run(self):
        with self.assertRaisesRegex(ValueError, 'Required test skipped'):
            self.check([{'Action': 'pass', 'Package': 'config', 'Test': 'TestConfig'},
                        {'Action': 'skip', 'Package': 'httpapi', 'Test': 'TestDatabase'}])

    def test_package_failure_is_not_hidden_by_passing_tests(self):
        with self.assertRaisesRegex(ValueError, 'Failed'):
            self.check([{'Action': 'pass', 'Package': 'config', 'Test': 'TestConfig'},
                        {'Action': 'fail', 'Package': 'httpapi'}])

    def test_empty_results_fail(self):
        with self.assertRaisesRegex(ValueError, 'No passing'):
            self.check([])

    def test_separately_executed_entry_point_is_allowed(self):
        self.check([{'Action': 'pass', 'Package': 'httpapi', 'Test': 'TestDatabase'},
                    {'Action': 'skip', 'Package': 'github.com/example-git/haunt-server/internal/clienttest', 'Test': 'TestClients'}])


if __name__ == '__main__':
    unittest.main()
