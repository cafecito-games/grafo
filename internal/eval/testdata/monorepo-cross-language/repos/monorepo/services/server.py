@app.post("/orders")
async def create_order(order_id: str):
    return order_id
