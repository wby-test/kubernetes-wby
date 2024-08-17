# 资源内部版本定义
## 资源内部类型 
types.go 
## 资源验证 
validation
## 资源注册 
register.go/install.go  ---> runtime.scheme
               |              ^
               |              |
               V              |  
            注册内部版本和外部版本到
                    

# 资源外部版本定义
> {v1 v1beta ...}

## 资源转换方法 （默认转换函数和自动生成转换函数）
## 资源默认值 （默认值函数和自动生成的默认值函数）
