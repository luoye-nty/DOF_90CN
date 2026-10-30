//------------------------------------------------------------------------------
// 手动声明的程序集特性（替代 SDK 自动生成的 AssemblyInfo）
// 原因：项目文件名 DNFLogin 与 AssemblyName(YH登录器) 不一致时，
// WPF wpftmp 临时项目与主项目共享 obj 目录会导致自动生成的特性重复(CS0579)。
// 此文件与 DNFLogin.csproj 中 GenerateAssemblyInfo=false / GenerateTargetFrameworkAttribute=false 配套。
//------------------------------------------------------------------------------

using System.Reflection;
using System.Runtime.Versioning;

[assembly: AssemblyCompany("落叶DNF登录器")]
[assembly: AssemblyConfiguration("Release")]
[assembly: AssemblyFileVersion("1.0.1.0")]
[assembly: AssemblyInformationalVersion("1.0.1")]
// AssemblyTitle/Product 按 Edition 分版（M2）：EXTERNAL/V90/VS4A21/VS4A12 由 csproj -p:Edition 注入
#if EXTERNAL && V90
[assembly: AssemblyProduct("落叶DNF登录器")]
[assembly: AssemblyTitle("落叶DNF登录器")]
#elif EXTERNAL && VS4A21
[assembly: AssemblyProduct("YH登录器-A21外网版")]
[assembly: AssemblyTitle("YH登录器-A21外网版")]
#elif EXTERNAL && VS4A12
[assembly: AssemblyProduct("YH登录器-A12外网版")]
[assembly: AssemblyTitle("YH登录器-A12外网版")]
#else
[assembly: AssemblyProduct("落叶DNF登录器")]
[assembly: AssemblyTitle("落叶DNF登录器")]
#endif
[assembly: AssemblyVersion("1.0.1.0")]
[assembly: TargetPlatform("Windows7.0")]
[assembly: SupportedOSPlatform("Windows7.0")]
