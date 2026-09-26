export async function submit(orderId: string) {
  return axios.post("/orders", orderId);
}
