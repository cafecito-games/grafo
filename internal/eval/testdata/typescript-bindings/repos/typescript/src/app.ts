import { RenamedService as Service, type Contract, direct } from "@app/lib/barrel"
import * as API from "./lib/barrel"
import { packaged } from "@fixture/tools"
import "missing-package"

export function run(contract: Contract, dynamic: any, union: Service | Contract): void {
  direct()
  API.direct()
  packaged()
  Service.create()
  const service = new Service()
  service.execute()
  contract.execute()
  dynamic.execute()
  union.execute()
}
