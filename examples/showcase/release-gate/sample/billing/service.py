def charge(subtotal, discount_cents, tax_basis_points):
    if any(
        type(value) is not int or value < 0
        for value in (subtotal, discount_cents, tax_basis_points)
    ):
        raise ValueError("Amounts must be nonnegative integers")
    if discount_cents > subtotal or tax_basis_points > 10000:
        raise ValueError("Invalid discount or tax")
    taxable = subtotal - discount_cents
    return taxable + (taxable * tax_basis_points + 5000) // 10000
