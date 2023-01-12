import {useAsync} from 'react-use'
import {FlightSQLDataSource} from '../datasource'
import {SelectableValue} from '@grafana/data'

type AsyncTablesState = {
  loadingTable: boolean
  tables: Array<SelectableValue<string>>
  errorTable: Error | undefined
}

export const GetTables = (datasource: FlightSQLDataSource): AsyncTablesState => {
  const result = useAsync(async () => {
    const res = await datasource.getTables()
    return res.frames[0].data.values[0].map((t: string) => ({
      label: t,
      value: t,
    }))
  }, [datasource])

  return {
    loadingTable: result.loading,
    tables: result.value ?? [],
    errorTable: result.error,
  }
}

export const buildQueryString = (
  columns: string,
  table: string,
  where: string | undefined,
  orderBy: string | undefined,
  groupBy: string | undefined,
  limit: string | undefined
): string => {
  let queryStr = `SELECT ${columns} FROM ${table}`

  if (where) {
    queryStr = queryStr + ` WHERE ${where}`
  }

  if (groupBy) {
    queryStr = queryStr + ` GROUP BY ${groupBy}`
  }

  if (orderBy) {
    queryStr = queryStr + ` ORDER BY ${orderBy}`
  }

  if (limit) {
    queryStr = queryStr + ` LIMIT ${limit}`
  }

  return queryStr
}

export const checkCasing = (str: string) => {
  const camelCase = /[A-Z]/.test(str)
  if (camelCase) {
    str = `"${str}"`
  }

  return str
}
