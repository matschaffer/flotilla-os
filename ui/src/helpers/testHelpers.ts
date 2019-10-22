import { FormikActions } from "formik"
import { createMemoryHistory, createLocation } from "history"
import { RouteComponentProps } from "react-router-dom"
import { Task, Run, RunStatus } from "../types"

export function createMockRouteComponentProps<MatchParams>({
  path,
  url,
  params,
}: {
  path: string
  url: string
  params: MatchParams
}): RouteComponentProps {
  return {
    history: createMemoryHistory(),
    match: {
      isExact: false,
      path,
      url,
      params,
    },
    location: createLocation(url),
  }
}

export const mockFormikActions: FormikActions<any> = {
  setStatus: jest.fn(),
  setError: jest.fn(),
  setErrors: jest.fn(),
  setSubmitting: jest.fn(),
  setTouched: jest.fn(),
  setValues: jest.fn(),
  setFieldValue: jest.fn(),
  setFieldError: jest.fn(),
  setFieldTouched: jest.fn(),
  validateForm: jest.fn(),
  validateField: jest.fn(),
  resetForm: jest.fn(),
  submitForm: jest.fn(),
  setFormikState: jest.fn(),
}

export const createMockTaskObject = (overrides?: Partial<Task>): Task => ({
  env: [{ name: "a", value: "b" }],
  arn: "arn",
  definition_id: "my_definition_id",
  image: "image",
  group_name: "group_name",
  container_name: "container_name",
  alias: "alias",
  memory: 1024,
  command: "command",
  tags: ["a", "b", "c"],
  ...overrides,
})

export const createMockRunObject = (overrides?: Partial<Run>): Run => ({
  instance: {
    dns_name: "my_dns_name",
    instance_id: "my_instance_id",
  },
  task_arn: "my_task_arn",
  run_id: "my_run_id",
  definition_id: "my_definition_id",
  alias: "my_alias",
  image: "my_image",
  cluster: "my_cluster",
  status: RunStatus.RUNNING,
  started_at: "started_at",
  group_name: "group_name",
  env: [],
  ...overrides,
})