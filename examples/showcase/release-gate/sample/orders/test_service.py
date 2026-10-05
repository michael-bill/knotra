import unittest
import service


class RegressionTests(unittest.TestCase):

    def test_multiple_lines(self):
        self.assertEqual(service.total([(1200, 2), (99, 1)]), 2499)

    def test_empty_order(self):
        self.assertEqual(service.total([]), 0)

    def test_zero_price(self):
        self.assertEqual(service.total([(0, 2)]), 0)

    def test_negative_price(self):
        with self.assertRaises(ValueError):
            service.total([(-1, 1)])

    def test_invalid_quantity(self):
        with self.assertRaises(ValueError):
            service.total([(100, 0)])

    def test_money_requires_integer_cents(self):
        with self.assertRaises(ValueError):
            service.total([(1.5, 2)])
