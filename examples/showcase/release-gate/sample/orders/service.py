def total(items):
    result = 0
    for price, quantity in items:
        if (
            type(price) is not int
            or type(quantity) is not int
            or price < 0
            or quantity < 1
        ):
            raise ValueError("Invalid line item")
        result += price * quantity
    return result
