export interface Contract {
  execute(): void
}

export class Service {
  static create(): Service { return new Service() }
  execute(): void {}
}

export default function direct(): void {}
