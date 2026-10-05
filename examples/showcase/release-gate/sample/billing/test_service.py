import unittest
import service


class RegressionTests(unittest.TestCase):

    def test_discount_before_tax(self):
        self.assertEqual(service.charge(1000, 100, 1000), 990)

    def test_zero_tax(self):
        self.assertEqual(service.charge(999, 10, 0), 989)

    def test_half_cent_rounds_up(self):
        self.assertEqual(service.charge(1, 0, 5000), 2)

    def test_full_discount(self):
        self.assertEqual(service.charge(100, 100, 2000), 0)

    def test_over_discount(self):
        with self.assertRaises(ValueError):
            service.charge(10, 11, 0)

    def test_negative_tax(self):
        with self.assertRaises(ValueError):
            service.charge(100, 0, -1)
