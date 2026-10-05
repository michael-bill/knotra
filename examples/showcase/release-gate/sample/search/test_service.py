import unittest
import service


class RegressionTests(unittest.TestCase):

    def test_case_folding(self):
        self.assertEqual(service.tokens("HELLO world"), ["hello", "world"])

    def test_punctuation(self):
        self.assertEqual(service.tokens("one,two!"), ["one", "two"])

    def test_duplicate_tokens(self):
        self.assertEqual(service.tokens("one one two"), ["one", "two"])

    def test_unicode_normalization(self):
        self.assertEqual(service.tokens("ＦＯＯ"), ["foo"])

    def test_empty_query(self):
        self.assertEqual(service.tokens("  "), [])

    def test_non_text(self):
        with self.assertRaises(ValueError):
            service.tokens(None)
