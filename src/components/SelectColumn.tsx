import React from 'react'

import {Select} from '@grafana/ui'

export const SelectColumn = ({columns, index, setColumn, column, formatCreateLabel}: any) => {
  columns && columns.forEach((c: any) => (c.index = index))
  return (
    <Select
      options={columns}
      onChange={setColumn}
      // isLoading={loadingColumn}
      // disabled={!!errorColumn}
      key={index}
      value={column.value}
      allowCustomValue={true}
      autoFocus={true}
      formatCreateLabel={formatCreateLabel}
      width={30}
      placeholder=""
    />
  )
}